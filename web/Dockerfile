# Wingman web — container image for the operator console.
#
# Two stages: a full Node toolchain to build, and a runtime image holding only what
# Next's standalone output traced. No npm install at run time, no devDependencies.
#
# Build from this directory:
#   docker build -t wingman/web:dev .
#
# The build needs network. app/layout.tsx uses next/font/google, which downloads Archivo
# and IBM Plex Mono at build time so the running console serves them from its own origin
# — a self-hosted dashboard that fetches a font from a CDN renders differently when the
# network is broken, which is exactly the moment somebody is reading it.
#
# Unlike the two Go images, nothing here is a decision-maker. This console holds no data,
# enforces no limit and keeps no state; it renders what the services say and forwards what
# an operator does. The only thing it guards is that a credential never reaches a browser,
# which is why every read happens in this process and not in the page.

# ---- Build ---------------------------------------------------------------
FROM node:22-alpine AS build

WORKDIR /src

# Dependencies resolve in their own layer, from the lockfile only, so editing a page does
# not re-resolve the tree. `npm ci` and not `npm install`: a container built from a
# different dependency graph than the one that was tested is not the thing that was tested.
COPY package.json package-lock.json ./
RUN npm ci

COPY . .

# next build also runs tsc over this context. A type error stops the image rather than
# shipping a console that fails on one route.
RUN npm run build

# ---- Runtime -------------------------------------------------------------
FROM node:22-alpine

# node:alpine already ships an unprivileged `node` user with a fixed uid. Nothing in this
# image writes to disk — there is no cache directory, no upload path and no workspace — so
# it needs no ownership beyond reading its own files.
WORKDIR /app

ENV NODE_ENV=production

# The traced server, then the static assets it serves. Two copies because standalone
# deliberately omits .next/static: it is content, not code, and a deployment behind a CDN
# would serve it from somewhere else.
COPY --from=build --chown=node:node /src/.next/standalone ./
COPY --from=build --chown=node:node /src/.next/static ./.next/static

# 0.0.0.0 inside the container, which is not the loopback default in .env.example. A
# process bound to 127.0.0.1 inside a container is unreachable from the network it is on,
# including from its own published port. Keeping the exposure loopback is the host's job,
# and deploy/docker-compose.yml does it by binding the published port to 127.0.0.1.
ENV HOSTNAME=0.0.0.0
ENV PORT=3000
EXPOSE 3000

# Secrets are not baked in. Every setting arrives through the environment; see
# .env.example, which is deliberately not copied into this image.

USER node

# /signin is the one page that reads neither service, so a 200 means this process is
# serving and its environment validated — not that the goal engine happens to be up. A
# probe that went through a page needing the engine would report the console as unhealthy
# every time something else broke.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:'+(process.env.PORT||3000)+'/signin').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"]

CMD ["node", "server.js"]
