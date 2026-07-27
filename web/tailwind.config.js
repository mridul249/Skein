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
        display: ['"Bricolage Grotesque"', 'system-ui', 'sans-serif'],
        sans: ['"IBM Plex Sans"', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'monospace'],
      },
      fontSize: {
        'display-l': ['40px', { lineHeight: '44px', letterSpacing: '-0.02em' }],
        'display-m': ['28px', { lineHeight: '34px', letterSpacing: '-0.015em' }],
        heading: ['18px', { lineHeight: '26px' }],
        body: ['15px', { lineHeight: '23px' }],
        label: ['13px', { lineHeight: '18px', letterSpacing: '0.01em' }],
        caption: ['12px', { lineHeight: '16px' }],
        data: ['13px', { lineHeight: '18px' }],
        'data-sm': ['11px', { lineHeight: '15px' }],
      },
      borderRadius: {
        // Design.md §4: one value, no exceptions.
        DEFAULT: '6px',
      },
      spacing: {
        sidebar: '240px',
        row: '44px',
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
