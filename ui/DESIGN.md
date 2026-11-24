# printscan UI design system

printscan runs one household printer and scanner. People reach the web UI from a phone in the hallway ("print this boarding pass"), from a laptop at a desk, and now and then from the homelab owner's screen while setting up the device. Each visit is a short task: print, scan, check why the printer is offline, change a default. Every screen is an Operate surface: the interface should get out of the way, look the same from screen to screen, and keep its personality to small details.

There is no PRODUCT.md yet. The product facts above come from the implementation plan. Nobody was interviewed for them.

## World: paper and process ink

The ground is cool office paper, the text is slate ink, and there is a single process-blue accent, the blue of a cyan cartridge mixed down to something you can read. Content sits on sheets: white cards on a slightly darker paper ground, each with a hairline edge and a soft two-layer shadow, like pages lying on a desk. Status colours appear only on the header dot, on badges, on banners and on marker meters (U19 colours those with the printer's own marker colours).

The paper is cool and slightly blue, the way copier paper looks under LED light. The dark theme turns the same system over: deep slate paper, light ink, and a lighter version of the same blue.

The theme follows `prefers-color-scheme`. People print in daylight and at night, so both themes get the same care.

## Tokens (`src/styles/tokens.css`)

### Colour roles

| Token                                   | Role                                                                      | Light                    | Dark                    |
| --------------------------------------- | ------------------------------------------------------------------------- | ------------------------ | ----------------------- |
| `--paper`                               | Page ground                                                               | `oklch(0.985 0.004 250)` | `oklch(0.19 0.018 255)` |
| `--paper-raised`                        | Cards, inputs, popovers, dialogs                                          | white                    | `oklch(0.235 0.02 255)` |
| `--paper-sunken`                        | Wells: drop zone, progress track, hover fill                              | `oklch(0.955 …)`         | `oklch(0.165 …)`        |
| `--rail`                                | Navigation rail and tab bar                                               | `oklch(0.965 …)`         | `oklch(0.21 …)`         |
| `--ink`                                 | Body text                                                                 | `oklch(0.24 0.025 255)`  | `oklch(0.94 …)`         |
| `--ink-muted`                           | Secondary text, descriptions (at least 4.5:1 on paper and raised paper)   | `oklch(0.45 …)`          | `oklch(0.78 …)`         |
| `--ink-faint`                           | Placeholders, idle icons, input borders on hover                          | `oklch(0.53 …)`          | `oklch(0.68 …)`         |
| `--rule` / `--rule-strong`              | Hairlines / control borders                                               |                          |                         |
| `--accent`                              | Primary buttons, selected controls, the active nav item                   | `oklch(0.5 0.15 250)`    | `oklch(0.72 0.13 245)`  |
| `--accent-tint`                         | Selected backgrounds (active nav, focused option, drop target)            |                          |                         |
| `--accent-text`                         | Links and quiet buttons                                                   |                          |                         |
| `--ok`, `--busy`, `--problem`, `--idle` | Status dots, borders and meters                                           | green, amber, red, slate |                         |
| `--*-tint`, `--*-text`                  | Banner and card backgrounds, and status text that passes contrast on them |                          |                         |
| `--focus-ring`                          | Every focus outline                                                       |                          |                         |
| `--selection`, `--scrim`                | Text selection, modal backdrop                                            |                          |                         |

Colours are always written as `oklch()`. Do not write raw hex in component or view CSS; reach for a role. When a view needs a colour that has no role yet, add the role here and in `tokens.css` first.

### Type

There is one family, Figtree Variable, self-hosted from `@fontsource-variable/figtree` (OFL-1.1). The scale is fixed rem with a ratio of 1.2. Only the page title is fluid, and it clamps between `--text-xl` and `--text-2xl`.

| Token                      | Size         | Use                                            |
| -------------------------- | ------------ | ---------------------------------------------- |
| `--text-xs`                | 13 px        | Tab bar labels                                 |
| `--text-sm`                | 14 px        | Labels, descriptions, badges, helper text      |
| `--text-md`                | 16 px        | Body, buttons, inputs                          |
| `--text-lg`                | 19.2 px      | Card titles, h2, the device name in the header |
| `--text-xl` / `--text-2xl` | 23 / 27.6 px | Page title (h1)                                |

Weights are `--weight-regular` 420, `--weight-medium` 540 (labels, buttons) and `--weight-strong` 650 (headings). Headings use `text-wrap: balance` and slight negative tracking. Body paragraphs stop at 70ch. Numbers in tables, meters and counts use `.tabular` (tabular figures). Monospace is for URIs and code only (`ipp://…`, eSCL URLs).

### Space, shape, size

- Space comes in 4 px steps, `--space-1` to `--space-8` (4, 8, 12, 16, 24, 32, 48, 64 px). Keep tight groups inside a card (8 to 12 px) and wider gaps between cards (16 to 24 px). Leave more room above a heading than below it.
- Radii: `--radius-control` 10 px for buttons, inputs and options; `--radius-sheet` 14 px for cards, dialogs and the drop zone; `--radius-pill` for dots, switches and the active tab-bar icon.
- Every interactive target is at least `--touch` (44 px) in both directions.
- Shell metrics: `--tabbar-height` 64 px, `--rail-width` 88 px, `--rail-width-wide` 240 px, `--content-max` 72 rem.
- Safe-area insets (`--safe-*`) pad the header, the tab bar, overlays and toasts (`viewport-fit=cover`).

### Motion

- Durations: `--dur-fast` 120 ms for hover and press, `--dur` 180 ms for state changes, `--dur-slow` 240 ms for dialogs and progress fills, all with `--ease-out` (an exponential ease-out).
- Motion only reports state: a popover or dialog opening, a switch flipping, a progress fill growing, and the one authored moment, the header dot breathing while the device is probing or deploying.
- Nothing animates on page load, and there are no entrance sequences.
- Under `prefers-reduced-motion: reduce` every duration token drops to 0 and the infinite animations (the breathing dot and the indeterminate bar) stop.

## Layers

`tokens.css` declares the order once: `@layer reset, tokens, base, components, views;`

- `reset`, `tokens` and `base` live in `src/styles/`. `base.css` also holds the app shell (`.app-*`).
- `components` is `src/components/ui.css` (`.ui-*`).
- `views` is each view's own `views/<View>.css`, imported by its component. View CSS goes inside `@layer views { … }`, uses only tokens, and puts its selectors under one root class per view (`.status-view …`), so views never fight each other.

## App shell (`src/app.tsx`)

- The header is sticky and slightly translucent. It shows the paper-sheet mark, the device name (the config's `name`, or the last good config's while the file is invalid, else "printscan") and a `StatusBadge` for the device phase: Ready, Starting (breathing), Needs attention, Not set up, or Offline when the event stream is down.
- Navigation has four destinations: Status, Print, Scan and Settings.
  - Under 768 px it is a bottom tab bar, and the current item gets a tinted pill behind its icon.
  - From 768 px it is a compact rail with the icon above the label.
  - From 1024 px it is a 240 px rail with labelled rows, and the current row is tinted.
- While the config file is missing, the shell hides the navigation and redirects every route to `/setup`.
- Banners sit above the page:
  - offline (amber, after 2.5 s without the stream);
  - invalid config (red, with the first error's line and message and a link to Settings).
- A skip link, the toast region, and focus moving to the page `h1` after each navigation.

## Components (`src/components/ui.tsx`)

They are styled wrappers over react-aria-components 1.22. They take their copy from props or `strings.ts` and never hard-code text.

| Component                                          | Notes                                                                                                                                                                                                                                                  |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `Button`                                           | Variants `primary` (one per view, for its main action), `secondary` (default), `quiet` (text-like, accent), `danger`. RAC states: hovered, pressed, disabled, pending.                                                                                 |
| `LinkButton`                                       | A link that looks like a button, for navigation and downloads.                                                                                                                                                                                         |
| `TextField`, `NumberField`, `Select`, `RadioGroup` | Label above, description and `errorMessage` below. `errorMessage` marks the field invalid. `Select` and `RadioGroup` take `options: {id, label, description?}[]`.                                                                                      |
| `Checkbox`, `Switch`                               | Use a checkbox for consent (EULAs) and a switch for an immediate on/off setting.                                                                                                                                                                       |
| `Dialog`                                           | A modal with a title and a Close button. `role="alertdialog"` drops outside-click dismissal. Use it only when the task needs protected focus: confirmations and conflicts. Prefer inline UI everywhere else. `children` may be `(close) => …`.         |
| `Tabs`, `TabList`, `Tab`, `TabPanel`               | Underline tabs.                                                                                                                                                                                                                                        |
| `ProgressBar`                                      | Determinate when given `value`, indeterminate otherwise.                                                                                                                                                                                               |
| `Meter`                                            | `color` takes the marker's own colour.                                                                                                                                                                                                                 |
| `toast()`, `ToastRegion`                           | Tones are neutral, ok and problem. Problems stay until dismissed; others leave after 5 s. The region sits above the tab bar on phones and bottom-right from 768 px.                                                                                    |
| `DropZone`, `FileTrigger`                          | Drop target (tinted while dragging) and file picker.                                                                                                                                                                                                   |
| `Disclosure`                                       | A heading-level-3 trigger with a chevron.                                                                                                                                                                                                              |
| `Card`                                             | A sheet with an optional title (h2 by default) and actions. It is a container-query container, so lay out its contents with `@container`. Never nest cards. `tone="problem"` is for errors that block the card's job.                                  |
| `StatusBadge`                                      | A dot and a text label (colour never carries meaning alone). Tones are ok, busy, problem and idle. `pulse` only while work is in flight.                                                                                                               |
| `PageHeader`                                       | Every view starts with one: the `h1` (the focus target after navigation), an optional description, and actions.                                                                                                                                        |
| `EmptyState`                                       | Says what will appear and how to make it appear, with an optional action.                                                                                                                                                                              |
| `InlineError`                                      | `role="alert"` with the problem and the recovery (`problemMessage(err)` from the client), plus an optional retry.                                                                                                                                      |
| `Icon`                                             | One authored 24 px set with a 1.75 stroke: status, print, scan, settings, chevronDown, check, plus, minus, close, alert, offline, upload, download, copy, file. Add new icons in the same grid and stroke. Never use emoji or Unicode glyphs as icons. |

## Copy

- The UI is English and written in sentence case. Shared copy lives in `src/strings.ts`, and each view keeps its own in `views/<View>.strings.ts`.
- Buttons name their action ("Save changes", "Print 2 copies"), and the toast that follows uses the same verb.
- Errors say what happened and what to do next. They do not apologise.
- Status words describe what the user sees or can do ("Waiting for the printer") and leave daemon names out.

## Browser surfaces

Text selection, the caret, `accent-color`, the scrollbar colour and focus rings all come from tokens. Focus is a 2 px `--focus-ring` outline with a 2 px offset on every interactive element. React Aria's `data-focus-visible` and `:focus-visible` get the same ring.

## Security constraints that shape the UI

The SPA runs under `default-src 'self'; img-src 'self' blob: data:`:

- no inline `<script>` or `<style>` (the build test enforces this);
- no `data:` fonts (the build copies the font files next to the CSS);
- no third-party origins;
- no `eval`: the generated schemas set `z.config({ jitless: true })` before any schema exists, so zod never probes `new Function`;
- no runtime `<style>` from react-aria: the `react-aria-pressable-style` marker in `index.html` stops `usePress` from injecting one, and `base.css` carries its `touch-action` rule instead.

On iOS Safari, react-aria's scroll lock injects an `overscroll-behavior` `<style>` while a modal is open. The CSP blocks it, so the browser logs a violation and only that overscroll tweak is lost. Chromium never takes that path.

Styles go in CSS files. Dynamic values go through React `style` props, which are set through the CSSOM and allowed by the CSP.
