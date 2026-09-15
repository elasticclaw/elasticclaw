# Chat visual port: t3code look → elasticclaw hub

Scope: VISUAL ONLY. No feature changes, no behavior changes, no new data flow. Every hook,
prop, memo boundary, anchor, windowing, typewriter, attachment, density/subagent toggle and
transcript copy must keep working exactly as today. Text content and labels stay the same
unless the spec below says otherwise. All code in English. Keep it simple and readable.

Reference repo (read-only, for exact values): /tmp/t3code-ref/apps/web/src
- index.css (tokens, .chat-markdown block, live-tool-shine, scrollbars, surface-glass)
- components/chat/MessagesTimeline.tsx (user/assistant rows, tool rows, working row, fold)
- components/ChatMarkdown.tsx (code block frame/header)
- components/chat/ComposerSurface.tsx, ChatComposer.tsx, ComposerPrimaryActions.tsx

Target app: /Users/anaberg/.t3/worktrees/elasticclaw/t3code-41a7c061/web (Next 16, React 19,
Tailwind v4 CSS-first, shadcn, lucide-react, react-markdown 9 + remark-gfm, shiki 4 present in deps).
`app/layout.tsx` hardcodes `<html class="dark">` — dark is the primary target; light tokens
must remain valid (never hardcode `bg-black/40`-style colors; use tokens).

## 1. Aesthetic thesis (from t3code)

"Keep controls and floating surfaces close to the neutral-black canvas. Borders and hover
states provide separation without milky gray fills." Concretely:
- Hairlines everywhere, fills almost nowhere. Borders at `border-border/60..80`; hover fills
  `bg-accent/20` (rows), `bg-muted/40` (expanded bodies).
- No avatars, no assistant bubble, no card per tool call, no colored success checkmarks,
  no persistent timestamps, no drop shadows in dark mode.
- Text hierarchy: `text-foreground` (headings) → `text-foreground/80` (prose) →
  `text-secondary-label` (tool labels) → `text-muted-foreground` (meta) →
  `text-muted-foreground/70` (resting controls) → `text-placeholder/75`.
- Icon sizes: `size-3` chevrons/copy, `size-3.5` control chevrons/alert icons,
  `size-4 stroke-[1.8]` tool activity icons. `tabular-nums` on every changing number.
- Text sizes: `text-sm` for message/tool/label text; `text-xs` meta; `text-[11px]` mono names
  and code headers; `text-[10px]` diff stats.
- Looping animations are `steps()` duty-cycled and respect `prefers-reduced-motion`.

## 2. Tokens to add/adjust in `web/app/globals.css`

Keep every existing var (other screens depend on them: charts, heatmap, status-*, step-*,
text-error/warning, sidebar). Do NOT lower `--muted-foreground` or `--border` contrast in
dark below what is there now (they were raised on purpose). Add:

```
:root {
  --control-radius: 0.5rem;
  --secondary-label: var(--muted-foreground);
  --icon-muted: var(--muted-foreground);
  --placeholder: var(--muted-foreground);
  --message-surface: var(--accent);                 /* user bubble */
  --message-foreground: var(--foreground);
  --message-action: oklch(0.488 0.217 264);          /* send button, light */
  --message-action-foreground: #fff;
  --message-action-hover: color-mix(in srgb, var(--message-action) 90%, var(--background));
  --error: var(--destructive);
  --error-foreground: oklch(0.505 0.213 27);          /* red-700-ish */
  --error-surface: color-mix(in srgb, var(--error) 8%, transparent);
  --warning: #f59e0b; --warning-foreground: #b45309;
  --warning-surface: color-mix(in srgb, var(--warning) 8%, transparent);
  --info-foreground: #1d4ed8;
  --tool-error-icon: var(--error);
  --diff-addition: var(--success); --diff-deletion: var(--destructive);
  --diff-addition-foreground: #059669; --diff-deletion-foreground: #dc2626;
  --code-background: color-mix(in srgb, var(--card) 90%, var(--background));
  --surface-raised: color-mix(in srgb, var(--card) 20%, transparent);
  --glass-blur: 12px; --glass-opacity: 80%; --glass-saturation: 1.14;
  --app-scrollbar-thumb: rgb(217 217 217); --app-scrollbar-thumb-hover: rgb(191 191 191);
}
.dark {
  --message-action: oklch(0.571 0.21 264);
  --error-foreground: #f87171; --tool-error-icon: #fca5a5;
  --error-surface: color-mix(in srgb, var(--error) 16%, transparent);
  --warning-foreground: #fbbf24;
  --warning-surface: color-mix(in srgb, var(--warning) 16%, transparent);
  --info-foreground: #60a5fa;
  --diff-addition-foreground: #34d399; --diff-deletion-foreground: #f87171;
  --surface-raised: color-mix(in srgb, var(--background) 97%, white);
  --glass-blur: 16px; --glass-saturation: 1.08;
  --app-scrollbar-thumb: rgb(255 255 255 / 8%); --app-scrollbar-thumb-hover: rgb(255 255 255 / 12%);
}
```
Dark palette nudges (t3 dark): `--secondary`, `--muted`, `--accent` may become translucent
whites (`rgb(255 255 255 / 3%)`, 3%, 4%) IF every consumer still reads correctly (sidebar
rows, board cards, analytics). If unsure keep the existing hex values; the chat must not
depend on those three being translucent. `--card` in dark → `color-mix(in srgb, var(--background) 97%, white)` is fine.

Expose in `@theme inline`: `--color-secondary-label`, `--color-icon-muted`, `--color-placeholder`,
`--color-message`, `--color-message-foreground`, `--color-message-action`,
`--color-message-action-foreground`, `--color-message-action-hover`, `--color-error`,
`--color-error-foreground`, `--color-error-surface`, `--color-warning`,
`--color-warning-foreground`, `--color-warning-surface`, `--color-info-foreground`,
`--color-tool-error-icon`, `--color-diff-addition`, `--color-diff-deletion`,
`--radius-2xl`, `--radius-3xl`, and `--radius-control: var(--control-radius)`.

Utilities to add (copy semantics from t3 index.css):
- `@utility surface-glass` (bg color-mix background/glass-opacity + backdrop blur/saturate, fallback without backdrop-filter).
- `@utility live-tool-shine` + `@keyframes live-tool-shine` (2.2s steps(30), text-clipped
  travelling highlight; reduced-motion → plain text). Simplify: no visibility gating var needed,
  just `animation-play-state` default running.
- Global scrollbar: 6px thumb using `--app-scrollbar-thumb`, radius 3px, transparent track
  (apply via existing `.scrollbar-thin` so other screens keep working; make `.scrollbar-thin`
  match t3's 6px look).
- `.chat-markdown` CSS block (see §5).

## 3. Chat layout (`ClawChatView` in `components/conversation-view.tsx`)

Keep the component tree and refs (`scrollRef`, `contentRef`, `bottomRef`, drag overlay,
header, NowStrip, TimelineToolbar, SubagentLanes, rail, composer, dialogs). Restyle:
- Header: 52px tall (`h-13`), no bottom border → keep a `border-b border-border/60` for now
  (the t3 mask fade is optional; if implemented use a `mask-image` gradient on the scroller
  top, 1.5rem). Title `font-mono text-sm font-medium text-foreground` (was text-xl semibold),
  meta in `text-xs text-muted-foreground tabular-nums`. Action buttons are ghost, `h-7 px-2
  text-xs rounded-[var(--control-radius)] text-secondary-label hover:text-foreground hover:bg-accent/40`.
  Kill stays visually destructive but as ghost text `text-destructive hover:bg-error-surface`.
- Scroller: `px-3 sm:px-5 overflow-y-auto scrollbar-thin overscroll-y-contain`,
  content column `mx-auto w-full max-w-3xl` with top spacer `h-3 sm:h-4`; rows spaced by
  per-kind bottom padding: user message `pb-4`, assistant `pb-2`, tool rows `pb-1`,
  fold/working `pb-1.5` (replace `space-y-4`; keep `contentRef` wrapping everything).
- Scroll-to-bottom pill: `surface-glass rounded-full border border-border/60 px-3 h-7 text-xs
  text-muted-foreground hover:text-foreground hover:border-border shadow-sm gap-1.5` with
  `ChevronDown size-3.5` and label "Scroll to end".
- Drag overlay: `bg-background/70 backdrop-blur-sm border border-dashed border-primary/60 rounded-[22px] m-3`.

## 4. Messages

### User (self) — `MessageBubble` role user, and card rows
```
row:    group flex flex-col items-end gap-1
bubble: relative max-w-[80%] rounded-2xl bg-message p-3 text-message-foreground text-sm leading-relaxed whitespace-pre-wrap
meta:   flex w-full max-w-[80%] items-center justify-end pe-1 text-xs tabular-nums text-muted-foreground
        opacity-0 transition-opacity duration-200 group-hover:opacity-100 focus-within:opacity-100 pointer-coarse:opacity-100
```
Long messages (>600 chars or >8 lines): clamp to `max-h-44` with
`maskImage: linear-gradient(to bottom, black calc(100% - 1.75rem), transparent)` and a ghost
`h-6 px-1.5 text-xs text-secondary-label hover:bg-muted/55` "Show full message"/"Show less"
button (local state only). Attachments (images) in `mb-2 grid max-w-[210px] grid-cols-2 gap-2`
frames `aspect-[4/3] overflow-hidden rounded-lg border border-border/80 bg-background/70`;
keep AttachmentChip API but restyle to those frames; text/file chips as `flex items-center
gap-2 py-1 text-sm` rows.

### Teammate
Same bubble as user but left-aligned (`items-start`), bg `bg-secondary`, name line above the
bubble `text-xs text-muted-foreground` with the small colored initials dot `size-4 rounded-full
text-[9px]` (keep person color). No tint per author beyond that.

### Assistant (claw)
NO bubble, NO Bot icon, NO claw-color tint. `relative min-w-0 px-1 py-0.5` full column width,
`<MarkdownContent className="chat-markdown">` with root `text-sm leading-relaxed text-foreground/80`.
Meta row below (`mt-1.5 flex items-center gap-2 text-xs tabular-nums text-muted-foreground`,
hover-revealed via `group/assistant`): a copy-message ghost button (`size-3` Copy → Check in
`text-primary`, 2s) and timestamp. Copy is a new tiny local affordance; it only copies that
message's text (no new data flow). Keep `sr-only` author heading `<h3>`.
`COLOR_CLASSES.bubble` stops being used for chat bubbles (leave the map in mappers.ts intact
for other consumers; if it becomes unused, leave it).

### Streaming (`StreamingMessage`)
Same assistant layout. Blocks fade in: root gets `data-streaming` and CSS
`.chat-markdown[data-streaming] > * { transition: opacity 600ms ease-out; @starting-style { opacity: 0 } }`
under `prefers-reduced-motion: no-preference`. Thinking dots → replace by one 24px row:
`Brain size-4 stroke-[1.8]` in a `size-6` box + "Thinking" with `live-tool-shine` (min-h-7).

### Hub / system notices, dividers, state rows
- Hub notice: `flex items-start gap-1.5 rounded-md px-1 py-0.5 text-xs text-muted-foreground`
  with `Settings2 size-3.5` in a `size-6` box; no background, no border, not italic.
- System divider / tool-gap / state row: t3 compaction divider →
  `role=separator flex items-center gap-3 py-1 text-xs text-muted-foreground`, hairlines
  `h-px flex-1 bg-border/70`, center label with `size-3` icon, sentence case (drop the
  uppercase tracking).

## 5. Markdown (`components/markdown-content.tsx`)

Port `.chat-markdown` CSS block into globals.css (rhythm 0.65rem, headings 1.25rem/1.125/1rem
with `font-weight:600; line-height:1.3; color: var(--foreground)`, lists with `--list-gutter`,
`li + li { margin-top: .25rem }`, inline code `border 1px var(--border); radius .375rem;
bg var(--muted); padding .1rem .35rem; font-size .75rem; color var(--foreground)`,
blockquote `border-left 2px var(--border); padding-left .8rem; color var(--muted-foreground)`,
tables `font-size .75rem; th/td padding .45rem .75rem; row hairlines only
color-mix(border 60%)` inside an `overflow-x-auto` container, links `color: var(--info-foreground)`
with the dotted-underline hover via `background-image: radial-gradient(circle, currentcolor .75px, transparent 1px)`
`background-size: 4px 2px; repeat-x; left bottom`, hr, images `rounded-lg border border-border/40 max-w-[min(100%,30rem)]`).
Remove inert `prose*` classes and the hardcoded `bg-black/40`.

Code block: a `CodeBlock` component (memo) —
```
frame:  chat-markdown-codeblock my-[0.65rem] overflow-hidden rounded-[var(--radius)] border border-border/70 bg-secondary leading-snug dark:border-transparent dark:bg-input/32
header: flex items-center justify-between gap-2 pt-1.5 pr-1.5 pb-0 pl-3 select-none
        label: font-mono text-[0.6875rem] text-foreground/72  (language name, e.g. "ts", "bash")
        actions: two icon buttons size-3 (WrapText toggle w/ aria-pressed, Copy → Check) color foreground/72 hover foreground; pressed bg foreground/8%
pre:    m-0 border-0 bg-transparent px-3 pb-3 pt-2 overflow-x-auto scrollbar-thin font-mono text-[13px] leading-relaxed
        wrap on: whitespace-pre-wrap [overflow-wrap:anywhere]
```
Highlighting: use shiki (already a dep) via dynamic `import("shiki")` with `codeToHtml`,
themes `github-light` / `github-dark` (pick by `document.documentElement.classList.contains("dark")`,
default dark), background forced transparent (`.chat-markdown .shiki { background: transparent !important }`),
memoized per (code, lang); render the plain `<pre><code>` while loading with the same height
(no layout jump), and fall back to plain on unknown language. Keep the singleton highlighter
cached at module level and load languages lazily; do not block first paint.
Keep `remark-gfm`, add GitHub alerts only if trivial (skip otherwise).

## 6. Tool activity (`components/agent-timeline/*`)

### Turn card → turn fold (turn-card.tsx)
No card, no border box, no muted header bar. A turn is: its user message row, then its
items, separated from the next turn by a hairline "fold" header:
```
<div class="border-b border-border/60 pb-2 pt-1">
  <button class="flex cursor-pointer select-none items-center gap-1 rounded-md px-1 text-sm leading-relaxed text-muted-foreground tabular-nums transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/70">
    <span>{label}</span><ChevronDown|ChevronRight class="size-3.5"/>
  </button>
</div>
```
Label examples: "Worked for 2m 14s · 12 tool calls" / "Working for 42s" (running; keep using
the existing clock hooks) / failures appended as "· 2 failed" in `text-destructive`. The
status pill with ping is removed; running state is conveyed by the "Working for …" label and
the shimmer on the live row. Keep the same expand/collapse state, anchor() call and memo.
Turn body: `flex flex-col` with the per-kind padding from §3, no `px-3 py-3`, no left accent.

### Step row (step-row.tsx) — quiet 24px rows
```
row:  flex flex-col rounded-md px-0.5 py-0.5 transition-colors [expandable: cursor-pointer hover:bg-accent/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/70] role=button tabIndex=0 aria-expanded
head: flex select-none items-center gap-1.5
icon: <span class="flex size-6 shrink-0 items-center justify-center text-icon-muted"><Icon class="size-4 shrink-0 stroke-[1.8]"/></span>
text: <p class="flex min-w-0 flex-1 items-baseline gap-1.5 text-sm leading-relaxed">
        <span class="shrink-0 text-secondary-label">{title}</span>
        <span class="min-w-0 flex-1 truncate text-secondary-label [font-mono when detailKind != text] text-[13px]">{detail}</span>
      </p>
right: duration `shrink-0 font-mono text-[.7rem] tabular-nums text-muted-foreground` (only when done), exit-code chip
       `rounded-sm border border-border/60 px-1 font-mono text-[.65rem] text-muted-foreground`,
       chevron `size-3 shrink-0 text-icon-muted opacity-70 transition-transform duration-200` + rotate-90 (invisible but space-reserved when not expandable)
```
Icons by category (lucide): read→Eye, edit→SquarePen, run→Terminal, search→Search, web→Globe,
task/subagent→Bot, other→Wrench, info→Check / warning→CircleAlert. Tones: failed →
icon `text-tool-error-icon/40` + title `text-secondary-label` (routine failures stay quiet);
warning → `text-warning` icon + `font-medium text-warning` title; severe error tone →
`text-destructive`. Remove all `border-l-2` accents and all literal `text-red-400`/`blue-500`.
Running row: label wrapped with `live-tool-shine` (no spinner, no ping).
Expanded body: `mt-1 ms-7 rounded-md bg-muted/40 px-3 py-2` containing
`<pre class="max-h-64 overflow-auto whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed text-secondary-label select-text cursor-text">`;
error output same box but text `text-error-foreground/80` (no red border). Clicks inside
stop propagation.
Card density (`variant="card"`) keeps the same look at `text-xs`, icon `size-3.5`, row `min-h-5`.

### Step group row ("Read 7 files")
Same 24px row anatomy with the category icon, summary in `text-secondary-label truncate`,
chevron; expanded list gets `ms-0 max-h-[min(18rem,50dvh)] overflow-y-auto scrollbar-thin rounded-md`
with a 1.5rem mask fade on the overflowing edge (`mask-image` top/bottom linear gradients).

### Activity summary block ("N earlier tool calls")
Same row anatomy: `History size-4` icon, label `text-secondary-label`, acts as a button;
no hairlines, no bordered pill. Card density: `text-xs`.

### Now strip (now-strip.tsx)
Full variant → the t3 "working row": `border-b border-border/60 pb-2 pt-1` +
`flex h-6 min-w-0 items-baseline gap-2 px-1 text-sm leading-relaxed text-muted-foreground tabular-nums`,
text "Working for {elapsed}" (keep the existing tick hook) + current step detail truncated in
`text-secondary-label live-tool-shine`; stale → `text-warning-foreground`; no green ping dot.
Card states: dot `size-1.5 rounded-full` with the semantic tokens (`bg-[var(--status-idle)]`
waiting, `bg-[var(--status-error)]` error, `bg-muted-foreground/50` done/offline), text in
`text-xs text-muted-foreground`, "Needs you" in `font-medium text-warning-foreground`.
Move it visually INSIDE the content column (max-w-3xl) rather than a full-width strip if the
tree allows without touching behavior; otherwise keep placement but restyle.

### Timeline toolbar (timeline-toolbar.tsx)
`flex items-center justify-between gap-3 px-3 sm:px-5 py-1.5` inside the column; segmented
control `inline-flex items-center gap-0.5 rounded-[var(--control-radius)] border border-border/60 p-0.5`
buttons `h-6 rounded-[calc(var(--control-radius)-2px)] px-2 text-xs text-secondary-label hover:text-foreground`
active `bg-accent text-foreground`; stats as plain `text-xs text-muted-foreground tabular-nums`
(no pill), failures `text-destructive`. Remove `border-b`.

### Subagents (subagent-*.tsx)
Rail card / lane card / finished card → t3 AgentSpawnRow anatomy: `rounded-md px-1 py-0.5
hover:bg-accent/20`, title `text-sm text-foreground/80 truncate`, role/model chip
`max-w-28 shrink-0 truncate rounded-sm border border-border/60 px-1 font-mono text-[.65rem] text-muted-foreground`,
status `shrink-0 font-mono text-[.7rem] tabular-nums text-muted-foreground`
(Working / Idle / Completed / Failed, or "1m 20s" after completion), second line
`truncate text-xs text-muted-foreground`. Keep `--subagent-wash` mechanism but make the wash
subtle (≤6%) and only for running. Rail: `w-[min(300px,22vw)] min-w-[196px] border-l border-border/60`,
sticky header `text-xs text-muted-foreground px-2.5 py-1.5` (drop uppercase tracking). Lanes
strip: same rows horizontally, `w-[220px] h-auto`. Subagent detail: task card →
`rounded-lg border border-border/70 bg-card/70 p-3`, result/error `<pre>` as in step body.
Subagent status dot keeps semantic colors; labels sentence case `text-[.7rem] font-mono`.

### Timeline list (timeline.tsx)
`flex flex-col` (no `space-y-3`), unloaded-activity notice restyled as a fold-style row;
"No failures" empty state `py-8 text-center text-sm text-muted-foreground`.

## 7. Composer (chat, in conversation-view.tsx) and card composer

Chat composer (keep form, textarea, hidden file input, paperclip, send, attachments,
`/stop` toast, auto-grow logic, `canSubmit`):
```
outer:   px-3 sm:px-5 pb-[calc(0.75rem+env(safe-area-inset-bottom))] pt-1.5 sm:pt-2 (no border-t)
shell:   group/composer relative isolate mx-auto w-full max-w-3xl rounded-[22px]
         bg-[color-mix(in_srgb,var(--card)_var(--glass-opacity),transparent)] backdrop-blur-[var(--glass-blur)]
         shadow-[0_12px_28px_-18px_rgb(0_0_0/40%)] dark:shadow-none
         after:pointer-events-none after:absolute after:inset-0 after:rounded-[inherit] after:border after:border-black/8 dark:after:border-white/5 dark:after:shadow-[inset_0_1px_rgb(255_255_255/3%)]
         focus-within: no ring change (whole surface is the field)
         drag-over: bg-accent/45 ring-1 ring-primary/70
inner:   rounded-[20px] px-3 pt-3.5 pb-2 sm:px-4 sm:pt-4
attachments strip (above the textarea, inside): mb-3 flex gap-2 flex-wrap; thumbs h-16 w-16 rounded-lg border border-border/80 overflow-hidden; remove button absolute right-1 top-1 bg-background/80
textarea: block w-full min-h-[70px] max-h-[200px] resize-none bg-transparent text-sm leading-relaxed text-foreground placeholder:text-placeholder/75 focus:outline-none border-0 ring-0
          (keep the JS auto-grow; adjust maxH to 200 and min to 70)
toast:   render as an attached banner ABOVE the shell: rounded-t-[16px] border border-b-0 border-warning/28 bg-warning-surface px-3 py-1.5 text-xs text-warning-foreground, overlapping the shell by 1rem+1px
footer:  flex items-center justify-between gap-2 px-3 pb-3 sm:px-4 sm:pb-4
   left: ghost controls `h-7 gap-1.5 px-2.5 rounded-[var(--control-radius)] text-secondary-label hover:text-foreground` — attach (Paperclip size-4) + a static hint "Enter to send · Shift+Enter newline" in text-xs text-muted-foreground/70 (hide on mobile)
   right: send button: h-9 w-9 sm:h-8 sm:w-8 rounded-full bg-message-action text-message-action-foreground shadow-xs transition-all duration-150 hover:scale-105 hover:bg-message-action-hover disabled:opacity-30 disabled:shadow-none disabled:hover:scale-100 enabled:inset-shadow-[0_1px_rgb(255_255_255/16%)] active:shadow-none
          glyph: 14x14 svg path "M7 11.5V2.5M7 2.5L3 6.5M7 2.5L11 6.5" strokeWidth 1.8 round caps (ArrowUp from lucide at size-3.5 is acceptable)
```
Board-card composer: same language at small scale — `rounded-2xl` shell with hairline,
transparent textarea `text-xs min-h-[36px]`, round send `size-7`.

## 8. Buttons / small primitives used by the chat
Ghost controls: `rounded-[var(--control-radius)] transition-colors text-secondary-label hover:text-foreground hover:bg-accent/40`.
Focus ring: `focus-visible:ring-2 focus-visible:ring-ring/70` (+ `ring-inset` on list rows).
CopyTranscriptButton / SubagentViewToggle / kill: follow §3 header controls.
Do not restyle shadcn `ui/*` primitives globally (ripples app-wide).

## 9. Verification
- `npm --prefix web run typecheck && npm --prefix web run lint` must pass.
- Visual: hub harness + next dev are running (see hack/chat-visual/README.md, script
  hack/chat-visual/screenshot.mjs). Capture with `--label after-<phase>` and READ the PNGs.
- Compare against the thesis in §1; iterate until it reads like t3code, not like the old cards.
