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

        // The account colour ramp, assigned in connection order. These are
        // an information system, not decoration: the same colour means the
        // same drive in the sidebar, the quota bar and every shard dot.
        drive1: '#89b4fa',
        drive2: '#a6e3a1',
        drive3: '#f9e2af',
        drive4: '#cba6f7',
        drive5: '#fab387',
        drive6: '#f5c2e7',
        drive7: '#94e2d5',
        drive8: '#eba0ac',
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
    },
  },
  plugins: [],
}
