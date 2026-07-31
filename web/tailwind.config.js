/**
 * Design.md §2-§4. Dark only, calm, comfortable density.
 *
 * Every colour here has a measured contrast ratio recorded in Design.md §2,
 * and the constraints between them — accounts vs semantics, accent vs
 * accounts — are asserted by the browser harness rather than trusted.
 */
/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}', './tests/browser/*.{ts,tsx,html}'],
  theme: {
    extend: {
      colors: {
        // ── Surface ladder ────────────────────────────────────────────────
        // Elevation is lightness plus a 1px border, never a shadow: shadows
        // barely read on dark, so a card that relies on one reads as flat.
        // Each step is ~2.5 dE2000 lighter than the last — enough to separate,
        // little enough that four levels still feel like one surface.
        // Slightly cool and near-neutral on purpose: the accent is violet, and
        // a violet-tinted grey would blunt it.
        canvas: '#0e1014', // page
        surface: '#15181e', // cards, table
        raised: '#1c2027', // hover, selected row, sunken wells
        overlay: '#232830', // menus, tooltips, dialogs

        // ── Lines ─────────────────────────────────────────────────────────
        // Borders do more separation work here than in light mode.
        line: '#262b34', // hairlines between rows and segments
        border: '#2f3540', // card and control borders
        borderStrong: '#3c4350', // inputs, where the edge must be findable

        // ── Text ──────────────────────────────────────────────────────────
        // Near-white, never #fff. Every tone below clears 4.5:1 on canvas,
        // surface and raised — including `faint`, because a placeholder is
        // still read. Muted grey on dark grey is the usual failure of this
        // style, so there is no tone here that fails it.
        text: '#e7eaf0', // 15.80 / 14.75 / 13.56
        muted: '#a4acbb', // 8.34 / 7.79 / 7.16
        faint: '#848da0', // 5.71 / 5.33 / 4.90

        // ── Accent ────────────────────────────────────────────────────────
        // One accent, used rarely: primary action, focus ring, current nav
        // item, links. Lightened and desaturated from the reference's vivid
        // violet, which vibrates at that saturation on a dark background.
        accent: '#c48af0', // 7.47 on canvas
        accentHover: '#d6aaf5',

        // ── Semantic ──────────────────────────────────────────────────────
        // Never also an account colour: every one is >= 20 dE2000 from all six
        // (known issue #29). Success is a true green rather than a mint,
        // because mint sat 16.8 dE from the teal account.
        success: '#6ddc8a',
        warning: '#eab464',
        danger: '#f2778a',

        // ── Account ramp ──────────────────────────────────────────────────
        // Assigned by the persisted `ordinal`, never by array position. Six,
        // not eight, and the NUMBER carries identity — colour is
        // reinforcement. See Design.md §5 and known issue #29.
        drive1: '#6fe2de',
        drive2: '#afd7de',
        drive3: '#a6c5e7',
        drive4: '#6f9de2',
        drive5: '#807dd4',
        drive6: '#b2afde',
      },

      fontFamily: {
        // Prose in a sans, data in a mono. Both vendored as woff2 subsets;
        // 90.2 KB total, inside the §8 budget. No CDN, ever.
        sans: ['"IBM Plex Sans"', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'monospace'],
      },

      fontSize: {
        // Comfortable, not dense. Line heights are generous because this is a
        // utility someone opens briefly and often, and tight leading makes a
        // glance cost more than it should.
        'display': ['28px', { lineHeight: '36px', letterSpacing: '-0.01em' }],
        'title': ['22px', { lineHeight: '30px', letterSpacing: '-0.005em' }],
        'heading': ['17px', { lineHeight: '24px' }],
        'body': ['15px', { lineHeight: '22px' }],
        'label': ['13px', { lineHeight: '18px' }],
        'caption': ['12px', { lineHeight: '16px' }],
        // Mono sizes run one step smaller at the same optical weight: a
        // monospace face reads larger than a sans at equal px.
        'data': ['13px', { lineHeight: '20px' }],
        'data-sm': ['12px', { lineHeight: '16px' }],
      },

      borderRadius: {
        // Modest, not zero and not pill. One step per element size.
        none: '0',
        sm: '4px', // chips, tags, the account mark
        DEFAULT: '6px', // buttons, inputs, table cells
        md: '8px', // cards, nav items
        lg: '12px', // panels, dialogs, drop zones
        full: '9999px', // only where a shape is genuinely circular
      },

      spacing: {
        sidebar: '248px',
        // Comfortable row height. The old 44px was terminal density; 52 gives
        // a 15px name and a 13px mono size room to sit without crowding.
        row: '52px',
        // For the day a "compact" toggle is added for long listings. Not wired
        // to anything yet — recorded so the value is decided once.
        'row-compact': '40px',
      },

      maxWidth: {
        content: '1280px',
        prose: '68ch',
      },

      transitionDuration: {
        hover: '120ms',
        panel: '180ms',
      },

      keyframes: {
        'modal-in': { from: { opacity: '0' }, to: { opacity: '1' } },
        // For work whose remaining duration is genuinely unknown. A solid
        // block sweeping a full-width track, not a gradient.
        'sweep': {
          '0%': { transform: 'translateX(-100%)' },
          '100%': { transform: 'translateX(400%)' },
        },
      },
      animation: {
        'modal-in': 'modal-in 180ms ease-out',
        sweep: 'sweep 1.4s ease-in-out infinite',
      },
    },
  },
  plugins: [],
}
