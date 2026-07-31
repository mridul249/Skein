/**
 * Design.md §2. Catppuccin Mocha, dark only.
 *
 * This is not a shortcut palette: it is the default aesthetic vocabulary of
 * the audience this ships to, and its 16-colour accent ramp solves the
 * per-account colour problem in §5 without inventing one.
 */
/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        crust: '#11111b',
        mantle: '#181825',
        base: '#1e1e2e',
        surface0: '#313244',
        surface1: '#45475a',
        surface2: '#585b70',
        overlay0: '#6c7086',
        subtext0: '#a6adc8',
        text: '#cdd6f4',

        mauve: '#cba6f7',
        lavender: '#b4befe',
        green: '#a6e3a1',
        yellow: '#f9e2af',
        red: '#f38ba8',
        sapphire: '#74c7ec',

        // The account colour ramp, assigned by the persisted `ordinal`.
        // Six, not eight: see Design.md §5 and known issue #29. Identity is
        // carried by the NUMBER in the chip; hue is reinforcement, so six
        // well-separated calm colours beat eight crowded ones, and a seventh
        // account reusing colour 1 is harmless because the number differs.
        //
        // Every one is >= 20 dE2000 from success green, warning amber and
        // error red in normal vision, and outside the violet accent band.
        drive1: '#6fe2de',
        drive2: '#afd7de',
        drive3: '#a6c5e7',
        drive4: '#6f9de2',
        drive5: '#807dd4',
        drive6: '#b2afde',
      },
      fontFamily: {
        // Design.md §3, after the Phase 7 Task 4.2 option A decision: one
        // face. `display` and `sans` are kept as aliases rather than deleted
        // so a stray `font-display` cannot silently fall back to system sans.
        display: ['"JetBrains Mono"', 'ui-monospace', 'monospace'],
        sans: ['"JetBrains Mono"', 'ui-monospace', 'monospace'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'monospace'],
      },
      fontSize: {
        // Every line height is a whole multiple of the 1.4rem baseline, so
        // mixed sizes still land on one grid. Letter-spacing is gone: a
        // monospace face has a fixed advance and tracking it fights the grid.
        'display-l': ['40px', { lineHeight: '2.8rem' }],
        'display-m': ['28px', { lineHeight: '2.8rem' }],
        heading: ['18px', { lineHeight: '1.4rem' }],
        body: ['15px', { lineHeight: '1.4rem' }],
        label: ['13px', { lineHeight: '1.4rem' }],
        caption: ['12px', { lineHeight: '1.4rem' }],
        data: ['13px', { lineHeight: '1.4rem' }],
        'data-sm': ['11px', { lineHeight: '1.4rem' }],
      },
      borderRadius: {
        // Design.md §4: radius 0 everywhere, no exceptions. Every named key
        // Tailwind ships is overridden, so `rounded-full` and friends cannot
        // reintroduce a curve by reaching past DEFAULT.
        none: '0',
        sm: '0',
        DEFAULT: '0',
        md: '0',
        lg: '0',
        xl: '0',
        '2xl': '0',
        '3xl': '0',
        full: '0',
      },
      spacing: {
        sidebar: '240px',
        row: '44px',
        // Horizontal rhythm is measured in characters, not pixels: padding
        // that borders text is a whole number of cells, so a label and the
        // text beside it share one grid. Vertical rhythm stays in rem
        // multiples of the 1.4rem baseline.
        '1ch': '1ch',
        '2ch': '2ch',
        '3ch': '3ch',
        '4ch': '4ch',
        '1line': '1.4rem',
        halfline: '0.7rem',
      },
      maxWidth: {
        content: '1400px',
      },
      transitionDuration: {
        hover: '120ms',
        panel: '180ms',
      },
      keyframes: {
        // Design.md §6: 180ms on panel open, and no travel worth calling a
        // transition — the dialog arrives, it does not slide in from
        // somewhere. Applied via motion-safe:, so it does not exist at all
        // under prefers-reduced-motion.
        'modal-in': {
          from: { opacity: '0' },
          to: { opacity: '1' },
        },
      },
      animation: {
        'modal-in': 'modal-in 180ms ease-out',
      },
    },
  },
  plugins: [],
}
