use std::collections::HashMap;
use std::sync::Mutex;

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// Trait for pluggable agent memory backends.
pub trait Memory: Send + Sync {
    /// Store a memory entry.
    ///
    /// # Errors
    /// Returns error if the storage backend fails.
    fn store(&self, entry: MemoryEntry) -> anyhow::Result<()>;

    /// Recall memories relevant to a query, returning up to `limit` entries.
    ///
    /// # Errors
    /// Returns error if the retrieval backend fails.
    fn recall(&self, query: &str, limit: usize) -> anyhow::Result<Vec<MemoryEntry>>;

    /// Forget (remove) a memory entry by ID.
    ///
    /// # Errors
    /// Returns error if the deletion backend fails.
    fn forget(&self, id: &str) -> anyhow::Result<()>;
}

/// A single memory entry stored by an agent.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MemoryEntry {
    pub id: String,
    pub content: String,
    pub memory_type: MemoryType,
    pub relevance: f32,
    pub metadata: HashMap<String, String>,
    pub created_at: DateTime<Utc>,
    pub last_accessed: DateTime<Utc>,
}

impl MemoryEntry {
    /// Create a new memory entry with the given content and type.
    pub fn new(content: impl Into<String>, memory_type: MemoryType) -> Self {
        let now = Utc::now();
        Self {
            id: Uuid::new_v4().to_string(),
            content: content.into(),
            memory_type,
            relevance: 0.0,
            metadata: HashMap::new(),
            created_at: now,
            last_accessed: now,
        }
    }

    /// Add metadata key-value pair (builder pattern).
    #[must_use]
    pub fn with_metadata(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.metadata.insert(key.into(), value.into());
        self
    }
}

/// Classification of memory entries.
///
/// The first three variants (`ShortTerm`, `LongTerm`, `Entity`) are
/// scope-based — they describe *when* a memory is valid and don't
/// carry intent.
///
/// The lower four variants (`User`, `Feedback`, `Project`, `Reference`)
/// are intent-based and adapted from the Claude Code v2.1.120 memory
/// subsystem. They tell future calls *how* to use the memory:
///
/// - `User` — durable facts about the user (role, expertise,
///   preferences). Shapes how to address and explain things.
/// - `Feedback` — corrections or confirmations on approach. Highest
///   leverage type — re-reading prevents re-litigating decisions.
/// - `Project` — current state of in-flight work — initiatives,
///   deadlines, who's doing what. Decays fast; verify against current
///   state before acting.
/// - `Reference` — pointers to where information lives outside this
///   project (Linear, Slack channel, dashboard URL, etc.).
///
/// Intent typing matters for *recall*: `Feedback` and `User` entries
/// stay applicable across sessions, while `Project` and `Reference`
/// need a freshness check before being acted on.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum MemoryType {
    /// Transient, session-scoped memory.
    ShortTerm,
    /// Persisted across sessions.
    LongTerm,
    /// Named entity or concept.
    Entity,
    /// Durable facts about the user — role, expertise, preferences.
    User,
    /// Corrections or validations on approach. Read these especially
    /// carefully; they're meant to prevent repeated drift.
    Feedback,
    /// Current state of in-flight work. Decays quickly; verify against
    /// current state before acting on it.
    Project,
    /// Pointer to where information lives outside this project
    /// (Linear, Slack, Grafana, GitHub, etc.).
    Reference,
}

impl MemoryType {
    /// True if recall sites should append a freshness-check nudge to
    /// any reminder rendered from a memory of this type.
    ///
    /// `Project` and `Reference` memories are time-sensitive — a
    /// claimed function path may have been renamed, an external
    /// dashboard URL may have moved. The other types ride on durable
    /// truths and don't need the same caveat.
    #[must_use]
    pub fn needs_freshness_check(self) -> bool {
        matches!(self, Self::Project | Self::Reference)
    }
}

/// Lowercased alphanumeric tokens. Punctuation is a separator, NOT part of a
/// token — scoring used to split on whitespace alone, so a query ending
/// "…my name?" never matched a memory containing "name" and the entry was
/// silently not recalled.
fn tokens(text: &str) -> Vec<String> {
    text.to_lowercase()
        .split(|c: char| !c.is_ascii_alphanumeric())
        .filter(|s| !s.is_empty())
        .map(str::to_string)
        .collect()
}

/// Fraction of the QUERY's distinct tokens that appear in `content`.
///
/// Normalised to 0.0–1.0 deliberately: a raw overlap count is comparable neither
/// between entries nor between languages, and it is rendered into the prompt.
/// This is the **cross-language contract** — the C#, Python, TypeScript and Go
/// cores compute the identical number (pearl th-ffaeae).
#[must_use]
pub fn relevance_score(query: &str, content: &str) -> f32 {
    let query_tokens: std::collections::BTreeSet<String> = tokens(query).into_iter().collect();
    if query_tokens.is_empty() {
        return 0.0;
    }
    let content_tokens: std::collections::BTreeSet<String> = tokens(content).into_iter().collect();
    let matching = query_tokens.iter().filter(|t| content_tokens.contains(*t)).count();
    #[allow(clippy::cast_precision_loss)]
    {
        matching as f32 / query_tokens.len() as f32
    }
}

/// Header that opens every auto-recall block, in every core.
pub const RECALL_HEADER: &str = "[Recalled memories]";

/// The verify-before-recommend note (rule D6), emitted only when at least one
/// recalled entry is time-sensitive ([`MemoryType::needs_freshness_check`]).
///
/// A memory naming a function, file, or flag is a claim about the PAST, not a
/// fact about now — without this line the model happily recommends a symbol that
/// was deleted three releases ago.
pub const RECALL_FRESHNESS_NOTE: &str = "Note: 'the memory says X exists' is not the same as 'X exists now'. \
    Before recommending or acting on any function path, file, flag, or external \
    pointer named below, verify it's current by reading the file or grepping the \
    codebase. Project and Reference memories are time-sensitive; User and Feedback \
    are durable.";

/// Render recalled entries as the context block injected into a turn.
///
/// This function is the **cross-language spec** for auto-recall: every sibling
/// core (C#, Python, TypeScript, Go) reproduces this exact text, so the same
/// memories yield byte-identical context on all five engines. Change it here and
/// the other four must follow — see `pearl th-ffaeae`, which exists because they
/// had drifted into three different spellings of the header alone.
///
/// Returns `None` for an empty slice, so a caller injects nothing rather than a
/// bare header.
#[must_use]
pub fn render_recall_block(entries: &[MemoryEntry]) -> Option<String> {
    use std::fmt::Write;

    if entries.is_empty() {
        return None;
    }
    let mut buf = String::from(RECALL_HEADER);
    buf.push('\n');
    if entries.iter().any(|e| e.memory_type.needs_freshness_check()) {
        buf.push_str(RECALL_FRESHNESS_NOTE);
        buf.push('\n');
    }
    for entry in entries {
        let _ = writeln!(buf, "- ({:?}, relevance={:.2}): {}", entry.memory_type, entry.relevance, entry.content);
    }
    Some(buf)
}

/// In-memory implementation of the `Memory` trait.
///
/// Uses a `Mutex<Vec<MemoryEntry>>` for thread-safe storage.
/// Recall performs keyword matching: splits the query into words and scores
/// entries by the number of matching words found in the content.
pub struct InMemoryMemory {
    entries: Mutex<Vec<MemoryEntry>>,
}

impl InMemoryMemory {
    pub fn new() -> Self {
        Self {
            entries: Mutex::new(Vec::new()),
        }
    }
}

impl Default for InMemoryMemory {
    fn default() -> Self {
        Self::new()
    }
}

impl Memory for InMemoryMemory {
    fn store(&self, entry: MemoryEntry) -> anyhow::Result<()> {
        let mut entries = self.entries.lock().map_err(|e| anyhow::anyhow!("lock poisoned: {e}"))?;
        entries.push(entry);
        Ok(())
    }

    fn recall(&self, query: &str, limit: usize) -> anyhow::Result<Vec<MemoryEntry>> {
        let entries = self.entries.lock().map_err(|e| anyhow::anyhow!("lock poisoned: {e}"))?;

        let mut scored: Vec<(f32, MemoryEntry)> = entries
            .iter()
            .filter_map(|entry| {
                let score = relevance_score(query, &entry.content);
                if score > 0.0 {
                    let mut recalled = entry.clone();
                    recalled.relevance = score;
                    Some((score, recalled))
                } else {
                    None
                }
            })
            .collect();

        // Best first; `sort_by` is stable, so ties keep insertion order — the
        // sibling cores rely on that to produce the same block.
        scored.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap_or(std::cmp::Ordering::Equal));
        scored.truncate(limit);

        Ok(scored.into_iter().map(|(_, entry)| entry).collect())
    }

    fn forget(&self, id: &str) -> anyhow::Result<()> {
        let mut entries = self.entries.lock().map_err(|e| anyhow::anyhow!("lock poisoned: {e}"))?;
        entries.retain(|e| e.id != id);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The block is the CROSS-LANGUAGE contract (th-ffaeae). Pinned byte-for-byte
    /// so a change here is a deliberate act that must be mirrored in the C#,
    /// Python, TypeScript and Go cores — not an accident someone notices months
    /// later when a conformance scenario can't be written.
    #[test]
    fn recall_block_is_pinned_across_languages() {
        let mut durable = MemoryEntry::new("brent prefers execution over questions", MemoryType::User);
        durable.relevance = 0.5;
        let block = render_recall_block(&[durable]).expect("block");
        assert_eq!(block, "[Recalled memories]\n- (User, relevance=0.50): brent prefers execution over questions\n");
    }

    /// A time-sensitive entry (Project/Reference) adds the verify-before-recommend
    /// note; a durable one (User/Feedback) must NOT, or the note becomes noise the
    /// model learns to skip.
    #[test]
    fn recall_block_adds_freshness_note_only_when_time_sensitive() {
        let mut project = MemoryEntry::new("the retry lives in fetch.rs", MemoryType::Project);
        project.relevance = 1.0;
        let block = render_recall_block(&[project]).expect("block");
        assert!(block.starts_with("[Recalled memories]\nNote: 'the memory says X exists'"));
        assert!(block.ends_with("- (Project, relevance=1.00): the retry lives in fetch.rs\n"));

        let mut user = MemoryEntry::new("prefers dark mode", MemoryType::User);
        user.relevance = 1.0;
        assert!(!render_recall_block(&[user]).expect("block").contains("Note:"));
    }

    /// Empty recall injects NOTHING — a bare header would spend context telling the
    /// model it remembered nothing.
    #[test]
    fn empty_recall_renders_no_block() {
        assert!(render_recall_block(&[]).is_none());
    }

    /// Relevance is a fraction of the QUERY's distinct tokens, so it is comparable
    /// across entries and across languages. Two of four query tokens hit ⇒ 0.50.
    /// Punctuation is a token separator. Scoring used to split on whitespace only,
    /// so "do you remember my name?" scored 0 against "the user's name is Dana"
    /// — the trailing '?' made `name?` fail a substring test — and the memory was
    /// silently never recalled.
    #[test]
    fn punctuation_does_not_defeat_a_match() {
        assert!(relevance_score("do you remember my name?", "The user's name is Dana.") > 0.0);
        assert!((relevance_score("watchlist!", "the watchlist lives here") - 1.0).abs() < f32::EPSILON);
    }

    #[test]
    fn relevance_is_a_fraction_of_query_words() {
        let memory = InMemoryMemory::new();
        memory
            .store(MemoryEntry::new("the watchlist lives on smoo-hub", MemoryType::Project))
            .expect("store");
        let hits = memory.recall("watchlist on marvin today", 5).expect("recall");
        assert_eq!(hits.len(), 1);
        assert!((hits[0].relevance - 0.5).abs() < f32::EPSILON, "relevance = {}", hits[0].relevance);
    }

    #[test]
    fn memory_entry_creation_and_serialization() {
        let entry = MemoryEntry::new("test content", MemoryType::ShortTerm).with_metadata("key", "value");

        assert_eq!(entry.content, "test content");
        assert_eq!(entry.memory_type, MemoryType::ShortTerm);
        assert_eq!(entry.metadata.get("key"), Some(&"value".to_string()));
        assert_eq!(entry.relevance, 0.0);

        let json = serde_json::to_string(&entry).expect("serialize");
        let parsed: MemoryEntry = serde_json::from_str(&json).expect("deserialize");
        assert_eq!(parsed.content, "test content");
        assert_eq!(parsed.memory_type, MemoryType::ShortTerm);
        assert_eq!(parsed.metadata.get("key"), Some(&"value".to_string()));
    }

    #[test]
    fn in_memory_store_and_recall() {
        let mem = InMemoryMemory::new();
        mem.store(MemoryEntry::new("rust programming language", MemoryType::LongTerm)).expect("store");
        mem.store(MemoryEntry::new("python data science", MemoryType::LongTerm)).expect("store");

        let results = mem.recall("rust", 10).expect("recall");
        assert_eq!(results.len(), 1);
        assert!(results[0].content.contains("rust"));
    }

    #[test]
    fn recall_keyword_matching_returns_relevant() {
        let mem = InMemoryMemory::new();
        mem.store(MemoryEntry::new("the quick brown fox jumps over the lazy dog", MemoryType::ShortTerm))
            .expect("store");
        mem.store(MemoryEntry::new("hello world program in rust", MemoryType::ShortTerm))
            .expect("store");
        mem.store(MemoryEntry::new("the fox is quick and clever", MemoryType::ShortTerm))
            .expect("store");

        let results = mem.recall("quick fox", 5).expect("recall");
        assert_eq!(results.len(), 2);
        // The entry with both words should score higher
        assert!(results[0].relevance >= results[1].relevance);
        assert!(results[0].content.contains("quick"));
    }

    #[test]
    fn recall_no_matches_returns_empty() {
        let mem = InMemoryMemory::new();
        mem.store(MemoryEntry::new("rust programming", MemoryType::ShortTerm)).expect("store");

        let results = mem.recall("javascript", 10).expect("recall");
        assert!(results.is_empty());
    }

    #[test]
    fn forget_removes_entry() {
        let mem = InMemoryMemory::new();
        let entry = MemoryEntry::new("to be forgotten", MemoryType::ShortTerm);
        let id = entry.id.clone();
        mem.store(entry).expect("store");

        assert_eq!(mem.recall("forgotten", 10).expect("recall").len(), 1);

        mem.forget(&id).expect("forget");
        assert!(mem.recall("forgotten", 10).expect("recall").is_empty());
    }

    #[test]
    fn memory_type_variants_serialize_correctly() {
        let types = [
            MemoryType::ShortTerm,
            MemoryType::LongTerm,
            MemoryType::Entity,
            MemoryType::User,
            MemoryType::Feedback,
            MemoryType::Project,
            MemoryType::Reference,
        ];
        for mt in &types {
            let json = serde_json::to_string(mt).expect("serialize");
            let parsed: MemoryType = serde_json::from_str(&json).expect("deserialize");
            assert_eq!(*mt, parsed);
        }

        let json = serde_json::to_string(&MemoryType::ShortTerm).expect("serialize");
        assert!(json.contains("ShortTerm"));
        let json = serde_json::to_string(&MemoryType::Feedback).expect("serialize");
        assert!(json.contains("Feedback"));
        let json = serde_json::to_string(&MemoryType::Reference).expect("serialize");
        assert!(json.contains("Reference"));
    }

    #[test]
    fn freshness_check_only_for_time_sensitive_types() {
        // D6: Project and Reference name external state that decays —
        // the agent must verify before recommending. User and Feedback
        // ride on durable truths and don't need the same caveat. Guards
        // against a refactor that flips a non-decaying type into the
        // freshness-check path (and bloats every recall block) or
        // drops Project/Reference out of it (and loses the recommend-
        // before-verify discipline).
        assert!(MemoryType::Project.needs_freshness_check());
        assert!(MemoryType::Reference.needs_freshness_check());
        assert!(!MemoryType::User.needs_freshness_check());
        assert!(!MemoryType::Feedback.needs_freshness_check());
        assert!(!MemoryType::ShortTerm.needs_freshness_check());
        assert!(!MemoryType::LongTerm.needs_freshness_check());
        assert!(!MemoryType::Entity.needs_freshness_check());
    }
}
