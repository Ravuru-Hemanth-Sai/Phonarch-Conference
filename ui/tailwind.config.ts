import type { Config } from "tailwindcss";

const config: Config = {
  content: ["./app/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        ink: "var(--text-primary)",
        muted: "var(--text-muted)",
        accent: "var(--accent)",
        surface: "var(--surface)",
      },
      boxShadow: { glass: "0 12px 34px rgba(31, 51, 62, 0.07)" },
    },
  },
  plugins: [],
};
export default config;
