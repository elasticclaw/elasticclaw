# Industry design system

The ElasticClaw console is a **dark technical wireframe**: steel-blue on a technical ground, square corners, transparent line-drawing panels with blueprint registration marks, condensed headings, monospace for machine text.

All theming lives in `app/globals.css` (Tailwind v4, CSS-first). The console is dark-only — `<html className="dark">` in `app/layout.tsx` — so `:root` and `.dark` carry the same values. The source system's light-ground palette is documented in the header comment of `globals.css` if a light mode is ever needed.

## Tokens

| Role | CSS var | Tailwind utility | When to use |
| --- | --- | --- | --- |
| Ground | `--background` `#16181a` | `bg-background` | The page and the sidebar. Nothing sits "under" it. |
| Text | `--foreground` `#eceded` | `text-foreground` | Default copy. |
| Panel | `--card` `transparent` | `bg-card` | Panels are line drawings — never a fill. |
| Floating surface | `--popover` / `--surface` `#1f2224` | `bg-popover` / `bg-surface` | Dialogs, menus, tooltips, sheets **and** input fields. Anything that overlaps content must be opaque. |
| Accent | `--primary` `#8fb4d8` | `bg-primary`, `text-accent-foreground` | The one accent hue. Primary button fill, active states, links, focus ring. |
| On-accent text | `--primary-foreground` `#16181a` | `text-primary-foreground` | Text on a solid accent fill. |
| Accent tint | `--accent` `rgba(143,180,216,.1)` | `bg-accent` | Nav-active and ghost hover tint — the tint, not the hue. |
| Muted text | `--muted-foreground` `#98989b` | `text-muted-foreground` | Captions, kickers, table headers, metadata. |
| Hairline | `--border` / `--input` `rgba(236,237,237,.2)` | `border-border` | Every border and divider in the system. |
| Focus ring | `--ring` `#8fb4d8` | `outline-ring` | 2px outline, offset 2. Applied globally in `@layer base`. |
| Danger | `--destructive` `#c96a6a` | `bg-destructive`, `text-destructive` | **The only non-accent hue.** Destructive actions and failure states only. |
| Accent ramp | `--color-industry-100..900` | `bg-industry-100`, `text-industry-800` | 100–300 tinted fills/hovers, 500 base, 700–900 text on those tints. Named `industry-*` because `--color-accent` is taken by the tint token. |
| Neutral ramp | `--color-neutral-100..900` | `bg-neutral-100`, `text-neutral-700` | Overrides Tailwind's default neutral palette with the Industry ramp — existing `neutral-*` usages resnap onto it, which is intended. |
| Status | `--color-status-{active,idle,offline,warning,failed}` | `bg-status-active`, `text-status-failed` | The **only** way to express agent/run state. |
| Unread | `--color-status-unread-{bg,fg}` | `bg-status-unread-bg` | The square unread counter. |
| Charts (categorical) | `--chart-1..5` | `fill-chart-1`, `bg-chart-1` | Deeper, more chromatic than the UI accent — chart marks need it (validated for lightness band, chroma, CVD and normal-vision separation on this ground). Fixed order: blue, teal, sand, violet, gray; assign in order, never cycled; slot 5 is the gray "Other" fold. Never color marks with the UI accent or the ramps. |
| Charts (status series) | `--chart-{positive,attention,warning,negative}` | `fill`/`stroke` via `var(--chart-positive)` | For series that ARE states (Clean / Human on the loop / Warning / Failed, Delivered / In progress / Failed). Same validated set, semantic assignment. |
| Heatmaps | `--heatmap-1..5` | `bg-heatmap-3` | Sequential single-hue steps from the accent ramp — magnitude, not identity. Funnels likewise stay single-hue (opacity steps of `chart-1`). |
| Radius | `--radius: 0px` (all steps) | `rounded-*` → 0 | Square by default. Round is opt-in via `rounded-full`. |
| Elevation | `--shadow-sm/md/lg` | `shadow-md`, `shadow-lg` | Floating layers only. Never an ad-hoc `box-shadow`. |
| Body font | `--font-sans` (Barlow) | `font-sans` | Body copy, 15px/1.55. |
| Heading font | `--font-heading` (Barlow Condensed 600) | `font-heading` | `h1`–`h6` (auto), buttons, KPI numbers. |
| Mono | `--font-mono` (system stack) | `font-mono` | Agent names, paths, ticket ids, numbers, durations, axis labels. |

## Do / Don't

**Do**

- Frame panels as blueprint objects — hairline border plus corner marks.
- Keep the grid visible: equal-width cells, strong horizontal and vertical rhythm.
- Condense headings; use mono for ids, paths and numbers.
- Express state through `status-*` tokens; take hovers and pressed states from the accent ramp.
- Use `tabular-nums` on any number that changes in place.

**Don't**

- Round cards, buttons, inputs, tags, dialogs or segmented controls. Only dots and avatars are round.
- Give a card or panel a surface fill — they are line drawings. The solid accent primary button is the single exception.
- Drop the registration marks from a framed element, or set `overflow-hidden` on a `Blueprint` (the marks sit outside the box; put the scroll container inside).
- Use a raw Tailwind palette class (`bg-green-500`, `text-red-400`, …). ESLint flags these.
- Add decorative color beyond the steel accent. Red is danger only.
- Use thick icon strokes — Lucide is pinned to stroke 1.5 globally.

## Primitive catalog

New primitives:

| Component | Import | Usage |
| --- | --- | --- |
| `Blueprint` | `@/components/ui/blueprint` | `<Blueprint className="p-4">…</Blueprint>` — bordered frame with four corner marks. Never `overflow-hidden`. |
| `Kicker` | `@/components/ui/kicker` | `<Kicker>Active agents</Kicker>`; `emphasis` switches it to accent. |
| `StatusDot` | `@/components/ui/status-dot` | `<StatusDot status="active" pulse />` — 7px dot, status-token colored. |
| `UnreadBadge` | `@/components/ui/status-dot` | `<UnreadBadge count={3} />` — square accent counter; renders nothing at 0. |
| `KpiGrid` / `KpiCell` | `@/components/ui/kpi` | `<KpiGrid columns={4}><KpiCell label="Runs" value="128" delta="+12 vs last week" /></KpiGrid>` |

Rethemed shadcn primitives (APIs unchanged):

- `Button` — `default` solid accent, `secondary`/`outline` hairline transparent, `ghost` accent text; heading font, square.
- `Badge` — `accent` / `neutral` / `outline` / `destructive`, 11px square. `default` and `secondary` remain as aliases.
- `Input`, `Textarea`, `Select` — surface fill, hairline border, min-height 36px, accent focus border.
- `Card` — transparent, hairline border, no shadow. Wrap in `Blueprint` for corner marks.
- `Dialog`, `Sheet`, `Popover`, `DropdownMenu`, `Tooltip` — opaque `bg-popover`, square, `shadow-lg`.
- `Tabs` — `TabsList` is the segmented control: hairline row, internal dividers, active trigger solid accent.
- `Table` — `TableHead` is kicker-styled, hairline row rules, subtle row hover.

## Adding a new screen or section

1. Start from the app shell — `bg-background`, no extra surface layers.
2. Wrap each panel, chart and form section in `<Blueprint>`; put scroll containers inside it.
3. Lead each panel with a `<Kicker>` above its heading; headings use the plain `h1`–`h6` tags.
4. Use semantic tokens and `industry-*`/`neutral-*` ramps for color, `status-*` for state, `font-mono` for ids and numbers.
5. Run `npm run lint` — the `no-restricted-syntax` rule catches raw palette colors and points back here.
