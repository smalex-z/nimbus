/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        // Mockup palette — peach / pink / lavender
        c1: '#F8AF82',
        c2: '#F496B4',
        c3: '#D2AAF0',
        c4: '#F8B496',
        ink: '#14121C',
        'ink-2': '#44424F',
        'ink-3': '#76747F',
        good: '#1F7A4D',
        warn: '#A85A0A',
        bad: '#B83A3A',
        line: 'rgba(20, 18, 28, 0.07)',
        'line-2': 'rgba(20, 18, 28, 0.13)',
      },
      fontFamily: {
        display: ['Fraunces', 'serif'],
        sans: ['Geist', 'system-ui', 'sans-serif'],
        mono: ['"Geist Mono"', 'ui-monospace', 'monospace'],
      },
      borderRadius: {
        DEFAULT: '10px',
        lg: '16px',
      },
      boxShadow: {
        glass:
          '0 1px 0 rgba(255,255,255,0.9) inset, 0 0 0 1px rgba(20,18,28,0.04), 0 12px 40px -12px rgba(20,18,28,0.10), 0 2px 6px -2px rgba(20,18,28,0.04)',
        'btn-primary': '0 8px 20px -8px rgba(20,18,28,0.5)',
      },
      keyframes: {
        drift: {
          '0%, 100%': { transform: 'translate(0,0) scale(1)' },
          '33%': { transform: 'translate(40px,-30px) scale(1.05)' },
          '66%': { transform: 'translate(-30px,40px) scale(0.97)' },
        },
        pulse: {
          '0%, 100%': {
            transform: 'scale(1)',
            boxShadow:
              '0 0 0 0 rgba(244, 150, 180, 0.4), inset 0 1px 2px rgba(255,255,255,0.8)',
          },
          '50%': {
            transform: 'scale(1.06)',
            boxShadow:
              '0 0 0 18px rgba(244, 150, 180, 0), inset 0 1px 2px rgba(255,255,255,0.8)',
          },
        },
        blink: {
          '0%, 100%': { opacity: '1' },
          '50%': { opacity: '0.3' },
        },
        fadeIn: {
          from: { opacity: '0', transform: 'translateY(8px)' },
          to: { opacity: '1', transform: 'translateY(0)' },
        },
      },
      animation: {
        drift: 'drift 22s ease-in-out infinite',
        pulse: 'pulse 2.4s ease-in-out infinite',
        blink: 'blink 1.2s ease-in-out infinite',
        fadeIn: 'fadeIn 0.4s ease',
      },
    },
  },
  plugins: [],
}
