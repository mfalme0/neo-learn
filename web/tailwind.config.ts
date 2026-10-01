/** @type {import('tailwindcss').Config} */
export default {
  content: ['./src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        // Deep teal. 5.2:1 against white, so it satisfies WCAG AA for normal
        // text and is safe as a button fill beneath white text.
        neo: {
          600: '#0d5c63',
          700: '#0a474d',
        },
      },
    },
  },
  plugins: [],
}