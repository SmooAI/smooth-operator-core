"""smooth-operator-core (Python): a native, in-process agent engine.

The Phase-0 Python sibling of the Rust reference engine and the C# core — an
agentic tool-calling loop over any OpenAI-compatible chat client, with in-memory
knowledge grounding. See ``docs/Architecture/Python Core.md``.
"""

from .agent import (
    AgentOptions,
    AgentRunResponse,
    DoneEvent,
    FunctionTool,
    SmoothAgent,
    StreamEvent,
    TextEvent,
    Tool,
    ToolCallEvent,
    ToolResultEvent,
    delegate_tool,
    effective_max_tokens,
)
from .cast import Cast, Clearance, OperatorRole, RoleKind
from .checkpoint import Checkpoint, CheckpointStore, InMemoryCheckpointStore
from .cost import CostBudget, CostTracker, ModelPricing, Usage
from .deny_policy import DenyPolicy, DenyPredicate, DenyReason, DenyRules
from .hooks import ToolCall, ToolHook, ToolResult
from .human_gate import (
    DelegateHumanGate,
    HumanApprovalRequest,
    HumanApprovalResponse,
    HumanDecision,
    HumanGate,
)
from .knowledge import InMemoryKnowledge, Knowledge, KnowledgeHit
from .llm_provider import (
    LlmProvider,
    MockLlmProvider,
    RecordedCall,
    text_response,
    tool_call_response,
    usage,
)
from .memory import InMemoryMemory, Memory, MemoryEntry
from .permission import (
    Allow,
    Ask,
    AutoMode,
    Category,
    Deny,
    PermissionHook,
    Verdict,
    covered_by_grants,
    decide,
    grant_query,
    tool_category,
)
from .permission_grants import (
    BashGrant,
    GrantQuery,
    NetworkGrant,
    PermissionGrants,
    SharedGrants,
    ToolGrant,
    append_grant,
    host_matches_glob,
    project_grants_path,
    user_grants_path,
)
from .rerank import LexicalReranker, NoopReranker, Reranker
from .thread import SmoothAgentThread
from .tool_search import ToolSearch
from .vector import Embedder, HashEmbedder, VectorKnowledge
from .workflow import END, Workflow, WorkflowError

__all__ = [
    "AgentOptions",
    "AgentRunResponse",
    "Cast",
    "DoneEvent",
    "StreamEvent",
    "TextEvent",
    "ToolCallEvent",
    "ToolResultEvent",
    "usage",
    "Checkpoint",
    "CheckpointStore",
    "Clearance",
    "CostBudget",
    "CostTracker",
    "DelegateHumanGate",
    "DenyPolicy",
    "DenyPredicate",
    "DenyReason",
    "DenyRules",
    "Embedder",
    "FunctionTool",
    "delegate_tool",
    "effective_max_tokens",
    "HashEmbedder",
    "HumanApprovalRequest",
    "HumanApprovalResponse",
    "HumanDecision",
    "HumanGate",
    "InMemoryCheckpointStore",
    "InMemoryKnowledge",
    "InMemoryMemory",
    "Knowledge",
    "KnowledgeHit",
    "LexicalReranker",
    "LlmProvider",
    "Memory",
    "MemoryEntry",
    "MockLlmProvider",
    "ModelPricing",
    "NoopReranker",
    "OperatorRole",
    "RecordedCall",
    "Reranker",
    "RoleKind",
    "SmoothAgent",
    "SmoothAgentThread",
    "Tool",
    "ToolCall",
    "ToolHook",
    "ToolResult",
    "ToolSearch",
    "Usage",
    "Allow",
    "Ask",
    "AutoMode",
    "Category",
    "Deny",
    "PermissionHook",
    "Verdict",
    "covered_by_grants",
    "decide",
    "grant_query",
    "tool_category",
    "BashGrant",
    "GrantQuery",
    "NetworkGrant",
    "PermissionGrants",
    "SharedGrants",
    "ToolGrant",
    "append_grant",
    "host_matches_glob",
    "project_grants_path",
    "user_grants_path",
    "VectorKnowledge",
    "Workflow",
    "WorkflowError",
    "END",
    "text_response",
    "tool_call_response",
]
