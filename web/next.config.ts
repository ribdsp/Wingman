import { fileURLToPath } from 'node:url'
import type { NextConfig } from 'next'

// Nothing clever here on purpose. This app is a thin operator console over two Go
// services, and every rule that matters is enforced in them.
const nextConfig: NextConfig = {
  reactStrictMode: true,
  // Traced dependencies only, copied into .next/standalone with a server.js. This is
  // what web/Dockerfile ships: a runtime image with no npm install in it and no
  // devDependencies, rather than the whole node_modules tree.
  output: 'standalone',
  // Pinned to this directory. Turbopack otherwise walks upwards looking for a lockfile
  // and can settle on one outside the repository entirely — a build whose root depends
  // on what happens to be in the parent directories is not a reproducible build.
  turbopack: { root: fileURLToPath(new URL('.', import.meta.url)) },
  // The console is served from behind whatever the operator already runs. It must
  // not advertise its own version to the internet.
  poweredByHeader: false,
  // A self-hosted tool has no business calling out to a font CDN at runtime, and a
  // dashboard that renders differently when the network is down is a dashboard that
  // lies about the network being down. Fonts are downloaded at build time by
  // next/font and served from this origin.
  images: { unoptimized: true },
  // Every credential lives on the server. If a client bundle ever imports a module
  // that reads one, the build should stop rather than ship it.
  serverExternalPackages: [],
  // `next dev` otherwise writes web/AGENTS.md and web/CLAUDE.md on every start. The
  // second one is read as project instructions by a coding agent working in this
  // repository, so a framework would be silently editing this project's own rules —
  // and both arrive as untracked noise in every diff. The guidance they carry is
  // already in the root CLAUDE.md, written deliberately.
  agentRules: false,
  async headers() {
    return [
      {
        source: '/:path*',
        headers: [
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'Referrer-Policy', value: 'no-referrer' },
          // The Content-Security-Policy is deliberately NOT here. It carries a
          // per-request nonce and is set in proxy.ts — see the comment there for why a
          // static one cannot work with a streamed React tree.
        ],
      },
    ]
  },
}

export default nextConfig
