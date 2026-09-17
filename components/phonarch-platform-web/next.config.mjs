/** @type {import('next').NextConfig} */
const controlApiOrigin = process.env.CONTROL_API_INTERNAL_URL ?? "http://127.0.0.1:8081";

const nextConfig = {
  output: "standalone",
  reactStrictMode: true,
  async rewrites() {
    return [
      { source: "/api/:path*", destination: `${controlApiOrigin}/api/:path*` },
    ];
  },
};
export default nextConfig;
