// Shared UI constants. Centralizes the magic values that were previously
// inlined in hooks/components (localStorage keys, polling intervals, cache
// limits, WebSocket backoff tuning).

// ─── localStorage keys ───────────────────────────────────────────────────────

/** Persisted manual ordering of the claw board (array of claw IDs). */
export const STORAGE_KEY_CLAW_ORDER = "elasticclaw_claw_order"

/** Persisted pinned flags per claw ID. */
export const STORAGE_KEY_PINNED = "elasticclaw_pinned"

/** Persisted per-claw message cache (durable messages only). */
export const STORAGE_KEY_MESSAGES = "elasticclaw_messages"

/** Last selected claw, restored on reload. */
export const STORAGE_KEY_SELECTED_CLAW = "elasticclaw_selected_claw"

// ─── Polling ─────────────────────────────────────────────────────────────────

/** How often the claw list is re-fetched. */
export const CLAW_POLL_INTERVAL_MS = 10_000

/**
 * How often dependency status is re-fetched. Slower-moving than claws and
 * separately cached by the hub.
 */
export const DEPENDENCY_POLL_INTERVAL_MS = 60_000

// ─── Message cache ───────────────────────────────────────────────────────────

/** Maximum durable messages persisted to localStorage per claw. */
export const MAX_CACHED_MESSAGES_PER_CLAW = 200

// ─── WebSocket reconnect ─────────────────────────────────────────────────────

/** First reconnect delay; doubles per attempt. */
export const WS_BASE_RECONNECT_DELAY_MS = 1_000

/** Ceiling for the exponential backoff delay. */
export const WS_MAX_RECONNECT_DELAY_MS = 30_000

/** Backoff exponent cap: delays stop growing after this many attempts. */
export const WS_MAX_BACKOFF_EXPONENT = 5

/** Minimum interval between logged WS error warnings. */
export const WS_ERROR_LOG_THROTTLE_MS = 10_000
