/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        ink: {
          DEFAULT: '#14121C',
          body: '#44424F',
          mute: '#76747F',
        },
        nimbus: {
          ok: '#1F7A4D',
          warn: '#A85A0A',
          err: '#B83A3A',
        },
      },
      fontFamily: {
        display: ['Fraunces', 'Georgia', 'serif'],
        sans: ['Geist', '-apple-system', 'BlinkMacSystemFont', 'Segoe UI', 'sans-serif'],
        mono: ['Geist Mono', 'SF Mono', 'Menlo', 'Consolas', 'monospace'],
      },
      borderRadius: {
        card: '18px',
      },
      boxShadow: {
        card: '0 1px 0 rgba(20,18,28,0.02), 0 30px 60px -30px rgba(20,18,28,0.12)',
      },
    },
  },
  plugins: [],
}
