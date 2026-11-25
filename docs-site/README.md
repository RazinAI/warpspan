# Warpspan docs

The public documentation for Warpspan, served at https://warpspan.docs.razin.ai.
Built with [Fumadocs](https://fumadocs.dev) on Next.js as a static export, set up
the same way as the Link and Deputy docs.

```sh
npm install
npm run dev          # http://localhost:3000
npm run build        # static site in out/
npx wrangler deploy  # publish (Cloudflare Worker with static assets)
```

Pages live in `content/docs` (MDX). Everything in them comes from the repo: the
README, `docs/decisions/`, `docs/protocol.md` and the code. Change them together.
