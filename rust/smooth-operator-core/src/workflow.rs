//! LangGraph-inspired typed workflow graph with conditional edges.
//!
//! Provides a state machine where nodes transform typed state and edges
//! (static or conditional) determine the next node to execute.

use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

use async_trait::async_trait;
use tracing::warn;

// ---------------------------------------------------------------------------
// State trait
// ---------------------------------------------------------------------------

/// Marker trait for workflow state — blanket-implemented for any
/// `Clone + Send + Sync + 'static` type.
pub trait State: Clone + Send + Sync + 'static {}
impl<T: Clone + Send + Sync + 'static> State for T {}

// ---------------------------------------------------------------------------
// Node trait
// ---------------------------------------------------------------------------

/// A node in the workflow graph.
#[async_trait]
pub trait Node<S: State>: Send + Sync {
    /// Unique name used to reference this node in edges.
    fn name(&self) -> &str;

    /// Execute the node, transforming `state` into a new state.
    async fn execute(&self, state: S) -> anyhow::Result<S>;
}

// ---------------------------------------------------------------------------
// FnNode helper
// ---------------------------------------------------------------------------

/// Type alias for the async closure used by [`FnNode`].
type AsyncNodeFn<S> = Box<dyn Fn(S) -> Pin<Box<dyn Future<Output = anyhow::Result<S>> + Send>> + Send + Sync>;

/// Convenience [`Node`] backed by an async closure.
pub struct FnNode<S: State> {
    name: String,
    func: AsyncNodeFn<S>,
}

impl<S: State> FnNode<S> {
    pub fn new(name: &str, func: impl Fn(S) -> Pin<Box<dyn Future<Output = anyhow::Result<S>> + Send>> + Send + Sync + 'static) -> Self {
        Self {
            name: name.to_string(),
            func: Box::new(func),
        }
    }
}

#[async_trait]
impl<S: State> Node<S> for FnNode<S> {
    fn name(&self) -> &str {
        &self.name
    }

    async fn execute(&self, state: S) -> anyhow::Result<S> {
        (self.func)(state).await
    }
}

// ---------------------------------------------------------------------------
// Sub-workflow node
// ---------------------------------------------------------------------------

/// Wrap a child [`Workflow`] as a single node of a parent workflow.
///
/// The child runs **to completion** — every node, its conditional edges and the
/// `__end__` sentinel included — inside one parent step. That is the contrast
/// with a conversational driver that advances the top-level graph one node per
/// user turn: a sub-workflow node executes its whole sub-graph within that one
/// turn, and the top level stays turn-gated.
///
/// The result is an ordinary [`Node`]: [`WorkflowBuilder::add_edge`] and
/// [`WorkflowBuilder::add_conditional_edge`] treat it exactly like a plain one,
/// as an edge source and as an edge target alike. Plain nodes and sub-workflows
/// are therefore interchangeable vertices of one composite graph, and they nest
/// arbitrarily — a sub-workflow may itself contain sub-workflows.
///
/// `map_in` projects parent state into the child's state type; `map_out` folds
/// the child's final state back into the parent's. An error from any child node
/// propagates out of the parent's [`Workflow::run`].
pub fn sub_workflow_node<P: State, C: State>(
    name: &str,
    child: Workflow<C>,
    map_in: impl Fn(&P) -> C + Send + Sync + 'static,
    map_out: impl Fn(P, C) -> P + Send + Sync + 'static,
) -> FnNode<P> {
    let child = Arc::new(child);
    let map_out = Arc::new(map_out);
    FnNode::new(name, move |state: P| {
        let child = Arc::clone(&child);
        let map_out = Arc::clone(&map_out);
        let child_state = map_in(&state);
        Box::pin(async move {
            let final_child = child.run(child_state).await?;
            Ok(map_out(state, final_child))
        })
    })
}

// ---------------------------------------------------------------------------
// Edge target
// ---------------------------------------------------------------------------

/// Target of an edge leaving a node.
pub enum EdgeTarget<S: State> {
    /// Go to a specific named node.
    Node(String),
    /// Route conditionally — the closure inspects state and returns the target
    /// node name, or `"__end__"` to terminate.
    Conditional(Box<dyn Fn(&S) -> String + Send + Sync>),
    /// End the workflow.
    End,
}

// ---------------------------------------------------------------------------
// Workflow
// ---------------------------------------------------------------------------

/// Sentinel value a conditional router can return to signal termination.
const END_SENTINEL: &str = "__end__";

/// A compiled workflow graph with typed state.
pub struct Workflow<S: State> {
    nodes: HashMap<String, Box<dyn Node<S>>>,
    edges: HashMap<String, EdgeTarget<S>>,
    entry: String,
    max_steps: usize,
}

impl<S: State> Workflow<S> {
    /// Execute the workflow starting from the entry node with `initial_state`.
    ///
    /// Returns the final state after reaching an [`EdgeTarget::End`] or the
    /// step limit.
    ///
    /// # Errors
    ///
    /// Returns an error if a node referenced by an edge does not exist, or if
    /// any node's `execute` method fails.
    pub async fn run(&self, initial_state: S) -> anyhow::Result<S> {
        let mut state = initial_state;
        let mut current = self.entry.clone();
        let mut visited: HashSet<String> = HashSet::new();

        for step in 0..self.max_steps {
            let node = self
                .nodes
                .get(&current)
                .ok_or_else(|| anyhow::anyhow!("node '{current}' not found in workflow"))?;

            // Detect revisit — warn but continue (loops can be intentional).
            if !visited.insert(current.clone()) {
                warn!(node = %current, step, "workflow revisited node (possible loop)");
            }

            state = node.execute(state).await?;

            // Determine next node.
            let Some(edge) = self.edges.get(&current) else {
                // No outgoing edge — implicit end.
                return Ok(state);
            };

            match edge {
                EdgeTarget::End => return Ok(state),
                EdgeTarget::Node(next) => {
                    current = next.clone();
                }
                EdgeTarget::Conditional(router) => {
                    let target = router(&state);
                    if target == END_SENTINEL {
                        return Ok(state);
                    }
                    current = target;
                }
            }
        }

        warn!(max_steps = self.max_steps, "workflow reached max steps limit");
        Ok(state)
    }
}

// ---------------------------------------------------------------------------
// Builder
// ---------------------------------------------------------------------------

/// Fluent builder for constructing a [`Workflow`].
pub struct WorkflowBuilder<S: State> {
    nodes: HashMap<String, Box<dyn Node<S>>>,
    edges: HashMap<String, EdgeTarget<S>>,
    entry: Option<String>,
    max_steps: usize,
}

impl<S: State> Default for WorkflowBuilder<S> {
    fn default() -> Self {
        Self::new()
    }
}

impl<S: State> WorkflowBuilder<S> {
    pub fn new() -> Self {
        Self {
            nodes: HashMap::new(),
            edges: HashMap::new(),
            entry: None,
            max_steps: 100,
        }
    }

    /// Register a node. Its [`Node::name`] is used as the key.
    pub fn add_node(mut self, node: impl Node<S> + 'static) -> Self {
        let name = node.name().to_string();
        self.nodes.insert(name, Box::new(node));
        self
    }

    /// Add a static edge from `from` to `to`.
    pub fn add_edge(mut self, from: &str, to: &str) -> Self {
        self.edges.insert(from.to_string(), EdgeTarget::Node(to.to_string()));
        self
    }

    /// Add a conditional edge whose router selects the next node at runtime.
    ///
    /// The router closure receives a `&S` and returns the target node name.
    /// Return `"__end__"` to terminate the workflow.
    pub fn add_conditional_edge(mut self, from: &str, router: impl Fn(&S) -> String + Send + Sync + 'static) -> Self {
        self.edges.insert(from.to_string(), EdgeTarget::Conditional(Box::new(router)));
        self
    }

    /// Set the entry node (first node to execute).
    pub fn set_entry(mut self, name: &str) -> Self {
        self.entry = Some(name.to_string());
        self
    }

    /// Mark `from` as a terminal node (its outgoing edge is [`EdgeTarget::End`]).
    pub fn set_end(mut self, from: &str) -> Self {
        self.edges.insert(from.to_string(), EdgeTarget::End);
        self
    }

    /// Override the maximum number of steps (default 100).
    pub fn max_steps(mut self, max: usize) -> Self {
        self.max_steps = max;
        self
    }

    /// Build the workflow, returning an error if misconfigured.
    ///
    /// # Errors
    ///
    /// Returns an error if no entry node was set or if the entry node name
    /// does not match any registered node.
    pub fn build(self) -> anyhow::Result<Workflow<S>> {
        let entry = self.entry.ok_or_else(|| anyhow::anyhow!("workflow has no entry node — call set_entry()"))?;

        if !self.nodes.contains_key(&entry) {
            anyhow::bail!("entry node '{entry}' not found in registered nodes");
        }

        Ok(Workflow {
            nodes: self.nodes,
            edges: self.edges,
            entry,
            max_steps: self.max_steps,
        })
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    /// Helper: create a FnNode that appends its name to a `Vec<String>` state.
    fn tracking_node(name: &str) -> FnNode<Vec<String>> {
        let n = name.to_string();
        FnNode::new(name, move |mut state: Vec<String>| {
            let n = n.clone();
            Box::pin(async move {
                state.push(n);
                Ok(state)
            })
        })
    }

    // 1. Linear workflow A → B → C executes in order
    #[tokio::test]
    async fn test_linear_workflow() {
        let wf = WorkflowBuilder::new()
            .add_node(tracking_node("a"))
            .add_node(tracking_node("b"))
            .add_node(tracking_node("c"))
            .add_edge("a", "b")
            .add_edge("b", "c")
            .set_entry("a")
            .set_end("c")
            .build()
            .unwrap();

        let result = wf.run(vec![]).await.unwrap();
        assert_eq!(result, vec!["a", "b", "c"]);
    }

    // 2. Conditional edge routes based on state
    #[tokio::test]
    async fn test_conditional_edge() {
        let wf = WorkflowBuilder::new()
            .add_node(tracking_node("start"))
            .add_node(tracking_node("left"))
            .add_node(tracking_node("right"))
            .add_conditional_edge("start", |state: &Vec<String>| {
                // The "start" node pushes "start", so len == 1
                if state.len() == 1 {
                    "right".to_string()
                } else {
                    "left".to_string()
                }
            })
            .set_entry("start")
            .set_end("left")
            .set_end("right")
            .build()
            .unwrap();

        let result = wf.run(vec![]).await.unwrap();
        assert_eq!(result, vec!["start", "right"]);
    }

    // 3. Workflow terminates at End
    #[tokio::test]
    async fn test_terminates_at_end() {
        let wf = WorkflowBuilder::new()
            .add_node(tracking_node("only"))
            .set_entry("only")
            .set_end("only")
            .build()
            .unwrap();

        let result = wf.run(vec![]).await.unwrap();
        assert_eq!(result, vec!["only"]);
    }

    // 4. Missing entry node returns build error
    #[tokio::test]
    async fn test_missing_entry_node_error() {
        let res = WorkflowBuilder::<Vec<String>>::new().build();
        let err = res.err().expect("expected build error");
        assert!(err.to_string().contains("no entry node"));
    }

    // 5. Max steps prevents infinite loop
    #[tokio::test]
    async fn test_max_steps_prevents_infinite_loop() {
        // A → B → A (loop)
        let wf = WorkflowBuilder::new()
            .add_node(tracking_node("a"))
            .add_node(tracking_node("b"))
            .add_edge("a", "b")
            .add_edge("b", "a")
            .set_entry("a")
            .max_steps(6)
            .build()
            .unwrap();

        let result = wf.run(vec![]).await.unwrap();
        // 6 steps: a, b, a, b, a, b
        assert_eq!(result, vec!["a", "b", "a", "b", "a", "b"]);
    }

    // 6. State is passed through and mutated by each node
    #[tokio::test]
    async fn test_state_mutation() {
        let add_ten = FnNode::new("add_ten", |state: i64| Box::pin(async move { Ok(state + 10) }));
        let double = FnNode::new("double", |state: i64| Box::pin(async move { Ok(state * 2) }));

        let wf = WorkflowBuilder::new()
            .add_node(add_ten)
            .add_node(double)
            .add_edge("add_ten", "double")
            .set_entry("add_ten")
            .set_end("double")
            .build()
            .unwrap();

        let result = wf.run(5).await.unwrap();
        assert_eq!(result, 30); // (5 + 10) * 2
    }

    // 7. Node failure propagates as workflow error
    #[tokio::test]
    async fn test_node_failure_propagates() {
        let fail_node = FnNode::new("fail", |_state: Vec<String>| Box::pin(async { anyhow::bail!("something went wrong") }));

        let wf = WorkflowBuilder::new().add_node(fail_node).set_entry("fail").set_end("fail").build().unwrap();

        let err = wf.run(vec![]).await.unwrap_err();
        assert!(err.to_string().contains("something went wrong"));
    }

    // 8. FnNode works with closure
    #[tokio::test]
    async fn test_fn_node_closure() {
        let prefix = "hello_".to_string();
        let node = FnNode::new("greet", move |state: String| {
            let prefix = prefix.clone();
            Box::pin(async move { Ok(format!("{prefix}{state}")) })
        });

        let wf = WorkflowBuilder::new().add_node(node).set_entry("greet").set_end("greet").build().unwrap();

        let result = wf.run("world".to_string()).await.unwrap();
        assert_eq!(result, "hello_world");
    }

    /// Helper: child graph of two tracking nodes joined by a conditional edge
    /// that skips a third node. Proves the whole sub-graph — routing included —
    /// runs inside the parent's single step.
    fn two_node_child() -> Workflow<Vec<String>> {
        WorkflowBuilder::new()
            .add_node(tracking_node("child_a"))
            .add_node(tracking_node("child_b"))
            .add_node(tracking_node("child_never"))
            .add_conditional_edge("child_a", |state: &Vec<String>| {
                if state.contains(&"child_a".to_string()) {
                    "child_b".to_string()
                } else {
                    "child_never".to_string()
                }
            })
            .set_entry("child_a")
            .set_end("child_b")
            .build()
            .unwrap()
    }

    // 9. A sub-workflow runs fully — 2 nodes + a conditional edge — in ONE parent step
    #[tokio::test]
    async fn test_sub_workflow_runs_to_completion_in_one_step() {
        let wf = WorkflowBuilder::new()
            .add_node(tracking_node("parent_a"))
            .add_node(sub_workflow_node("sub", two_node_child(), Vec::clone, |_parent, child| child))
            .add_node(tracking_node("parent_b"))
            .add_edge("parent_a", "sub")
            .add_edge("sub", "parent_b")
            .set_entry("parent_a")
            .set_end("parent_b")
            .build()
            .unwrap();

        let result = wf.run(vec![]).await.unwrap();
        assert_eq!(result, vec!["parent_a", "child_a", "child_b", "parent_b"]);
    }

    // 10. Parent and child states are different types — map in, map out
    #[tokio::test]
    async fn test_sub_workflow_state_maps_in_and_out() {
        let child = WorkflowBuilder::new()
            .add_node(FnNode::new("add_ten", |n: i64| Box::pin(async move { Ok(n + 10) })))
            .add_node(FnNode::new("double", |n: i64| Box::pin(async move { Ok(n * 2) })))
            .add_edge("add_ten", "double")
            .set_entry("add_ten")
            .set_end("double")
            .build()
            .unwrap();

        // Parent state is (label, total); the child only ever sees the i64.
        let wf = WorkflowBuilder::new()
            .add_node(sub_workflow_node(
                "math",
                child,
                |(_, total): &(String, i64)| *total,
                |(label, _): (String, i64), total: i64| (format!("{label}:done"), total),
            ))
            .set_entry("math")
            .set_end("math")
            .build()
            .unwrap();

        let result = wf.run(("start".to_string(), 5)).await.unwrap();
        assert_eq!(result, ("start:done".to_string(), 30));
    }

    // 11. A failing child node aborts the parent run
    #[tokio::test]
    async fn test_sub_workflow_error_propagates_to_parent() {
        let child = WorkflowBuilder::new()
            .add_node(FnNode::new("boom", |_state: Vec<String>| Box::pin(async { anyhow::bail!("child exploded") })))
            .set_entry("boom")
            .set_end("boom")
            .build()
            .unwrap();

        let wf = WorkflowBuilder::new()
            .add_node(tracking_node("parent_a"))
            .add_node(sub_workflow_node("sub", child, Vec::clone, |_parent, child| child))
            .add_node(tracking_node("parent_b"))
            .add_edge("parent_a", "sub")
            .add_edge("sub", "parent_b")
            .set_entry("parent_a")
            .set_end("parent_b")
            .build()
            .unwrap();

        let err = wf.run(vec![]).await.unwrap_err();
        assert!(err.to_string().contains("child exploded"));
    }

    // 12. Nesting deeper than one level — parent → child → grandchild
    #[tokio::test]
    async fn test_sub_workflow_nesting_depth_two() {
        let grandchild = WorkflowBuilder::new()
            .add_node(tracking_node("grand_a"))
            .add_node(tracking_node("grand_b"))
            .add_edge("grand_a", "grand_b")
            .set_entry("grand_a")
            .set_end("grand_b")
            .build()
            .unwrap();

        let child = WorkflowBuilder::new()
            .add_node(tracking_node("child_a"))
            .add_node(sub_workflow_node("grand", grandchild, Vec::clone, |_parent, child| child))
            .add_edge("child_a", "grand")
            .set_entry("child_a")
            .set_end("grand")
            .build()
            .unwrap();

        let wf = WorkflowBuilder::new()
            .add_node(tracking_node("parent_a"))
            .add_node(sub_workflow_node("sub", child, Vec::clone, |_parent, child| child))
            .add_edge("parent_a", "sub")
            .set_entry("parent_a")
            .set_end("sub")
            .build()
            .unwrap();

        let result = wf.run(vec![]).await.unwrap();
        assert_eq!(result, vec!["parent_a", "child_a", "grand_a", "grand_b"]);
    }

    // 13. Plain nodes and sub-workflows are interchangeable vertices: one composite
    //     graph wires both with the same conditional edges — routing INTO a
    //     sub-workflow and OUT of one — and the sub-workflow itself mixes a plain
    //     node with a nested sub-workflow (depth 2). All of it in one parent run.
    #[tokio::test]
    async fn test_composite_graph_mixes_plain_and_sub_workflow_vertices() {
        let deep = WorkflowBuilder::new()
            .add_node(tracking_node("deep_a"))
            .add_node(tracking_node("deep_b"))
            .add_node(tracking_node("deep_never"))
            .add_conditional_edge("deep_a", |state: &Vec<String>| {
                if state.contains(&"deep_a".to_string()) {
                    "deep_b".to_string()
                } else {
                    "deep_never".to_string()
                }
            })
            .set_entry("deep_a")
            .set_end("deep_b")
            .build()
            .unwrap();

        // A sub-workflow whose own vertices are a plain node AND a sub-workflow.
        let enrich = WorkflowBuilder::new()
            .add_node(tracking_node("enrich_a"))
            .add_node(sub_workflow_node("deep", deep, Vec::clone, |_parent, child| child))
            .add_edge("enrich_a", "deep")
            .set_entry("enrich_a")
            .set_end("deep")
            .build()
            .unwrap();

        // Parent: plain --conditional--> sub-workflow --conditional--> plain.
        let wf = WorkflowBuilder::new()
            .add_node(tracking_node("classify"))
            .add_node(sub_workflow_node("enrich", enrich, Vec::clone, |_parent, child| child))
            .add_node(tracking_node("finish"))
            .add_node(tracking_node("never"))
            .add_conditional_edge("classify", |state: &Vec<String>| {
                if state.contains(&"classify".to_string()) {
                    "enrich".to_string()
                } else {
                    "never".to_string()
                }
            })
            // A conditional edge LEAVING a sub-workflow vertex, routing on state the
            // sub-workflow produced — including the terminate sentinel.
            .add_conditional_edge("enrich", |state: &Vec<String>| {
                if state.contains(&"deep_b".to_string()) {
                    "finish".to_string()
                } else {
                    END_SENTINEL.to_string()
                }
            })
            .set_entry("classify")
            .set_end("finish")
            .build()
            .unwrap();

        let result = wf.run(vec![]).await.unwrap();
        assert_eq!(result, vec!["classify", "enrich_a", "deep_a", "deep_b", "finish"]);
    }
}
