# Local Homepage Style Comparison

## Production Promotion: November 4, 2026

The user authorized deployment of the selected refined homepage. The default
homepage now loads `homeAppearance.css`, the Electric Logo, and provider marks
in production. The comparison controls and A/B/C layouts remain DEV-only.
Only an explicit loopback `preview=home-polish` can select the old appearance;
production visitors cannot change it using preview or finish query parameters.
Custom HTML/URL content and compact home settings still take precedence.
The release is based on the verified live commit
`263ed0a58e6b14fbb63d89c91bb2fbd2a7d108ed`, retaining its media billing fixes.
The historical notes below describe the earlier local-only comparison stage.

The user selected the incumbent homepage shown in their screenshot, not A/B/C:
brand and terminal hero, three feature cards, model badges, and black Ghost Fibers.
Open `http://127.0.0.1:4187/home?preview=ghost-fibers-v2` for that selected local version.
The A/B/C comparison remains available as a reference. No deployment is authorized.
The existing black Ghost Fibers implementation is unchanged; all variants reuse it.
Switching variants preserves the same mounted background canvas.

## Incumbent Polish Comparison
- Open `http://127.0.0.1:4187/home?preview=home-polish&finish=refined`.
- Fixed bottom controls switch between the original and refined incumbent.
- `finish=original` opens the untouched incumbent styling; selection survives reload.
- Both finishes share the same markup, text, links, terminal animation, and fiber canvas.
- Refinement neutralizes terminal/card surfaces, reduces icon glow, adjusts spacing,
  and aligns provider tiles. It does not change the background configuration.
- The terminal keeps its original command, response, and reveal/cursor animation.
  A quiet violet-gray title bar, smaller window dots, explicit monospace font stack,
  restrained syntax colors, and a separated mint success response improve hierarchy.
  All terminal refinements remain scoped to the local refined finish.
- The primary CTA uses a brighter electric-lilac surface and a soft violet cast shadow
  in dark mode, with a 192x56px minimum target and bolder text for clearer prominence.
  Light mode keeps a deep plum surface. An explicit CJK sans-serif fallback, balanced
  arrow spacing, hover/pressed/focus states, and reduced-motion handling remain;
  label and destination stay unchanged. No looping attention animation is added.
- The three feature modules use an open layout: inline icons/headings, fine dividers,
  and aligned descriptions instead of separate boxed cards. Mobile stacks the same content.
- The feature band's tint and horizontal rules fade into the existing background
  at both ends; internal dividers also taper. Only decorative pseudo-elements fade,
  so text/icons stay sharp in both themes and the shared fiber layer is unchanged.
- Provider labels and support badges share one line. Muted lavender-gray borders,
  low-intensity violet glow, and matching support badges blend with the fiber
  background; model icons keep their brand colors. Hover gently strengthens the edge.
- Refined-only provider SVGs reuse the monitor icon library; the Antigravity
  silhouette comes from its brand asset. Original letter icons remain in comparison mode.
- The refined header uses the user's Electric Logo tracing/GLSL with their supplied
  violet settings. Native WebGL2 replaces only the React/OGL binding; no packages changed.
- The electric preview uses a transparent copy of the supplied QuotaFlow logo with
  the original lower-right watermark excluded. The public site logo is not modified.
  Both the shader and static fallback use the same clean asset.
- The header reserves room above/below the mark; a feathered canvas edge prevents
  the bloom from ending at a visible rectangle. The logo no longer extends above
  the viewport. The page background and electrical motion/color settings stay unchanged.
- After comparing the original 40px size, the current local preview uses an
  intermediate 80x48px header slot on desktop and mobile. A 112x80px transparent
  drawing surface leaves 16px of glow bleed; the supplied scale 0.7 renders the
  mark approximately 78px wide. The fallback compensates for the source PNG's whitespace.
- The logo retains the static image while loading or
  on failure/reduced motion, and pauses when hidden/offscreen. Theme changes keep the
  canvas reusable. Original comparison, compact/custom home and production are untouched.
- Activation requires DEV mode, a loopback hostname, and `preview=home-polish`.
- Controls and refinement CSS are loaded through a DEV-only async import.
- The previously selected `preview=ghost-fibers-v2` stays unchanged.
- The polish is a local proposal, not approval to deploy or replace production styling.

## Preview
- Open `http://127.0.0.1:4187/home?preview=styles&style=a`.
- Use `style=b` or `style=c`, or the fixed bottom A/B/C buttons.
- Selection updates the URL and survives reload; missing/invalid style defaults to A.
- A: left-aligned brand, two CTAs, and a capability strip.
- B: centered minimal title and model names, with product entry links below.
- C: staggered API and creative-workshop entries; stacked on mobile.

## Boundaries
- Activation requires DEV mode, a loopback hostname, and `preview=styles`.
- Production excludes the preview component through a DEV-only async import.
- Custom home content and compact home retain precedence; normal home stays unchanged.
- Workshop routing derives from the user-visible custom menu's exact label
  `\u521b\u610f\u5de5\u574a` (Creative Workshop), never a hardcoded menu ID.
- The local Vite config exposes only GET public settings upstream; write/auth API paths are blocked.
- GET setup status is answered locally. No production login or action testing is performed.

## Verification Handoff
- Focused suite: 39 tests, including polish gating, URL restoration, switching,
  canvas identity, theme preference preservation, and custom/compact precedence.
- Desktop 1440x1000 and mobile 390x844: no horizontal overflow.
- Keyboard, reload, capability anchor, login-page navigation, and reduced motion exercised.
- Screenshots: `frontend/output/playwright/home-styles`.
- Incumbent comparison screenshots: `frontend/output/playwright/home-polish`.
- This note does not authorize changes to production styling or existing product/design documents.
