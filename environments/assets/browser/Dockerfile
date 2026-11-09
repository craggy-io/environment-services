FROM node:24.11.0-bookworm@sha256:e5bbac0e9b8a6e3b96a86a82bbbcf4c533a879694fd613ed616bae5116f6f243
RUN npm install -g --ignore-scripts @playwright/mcp@0.0.84 playwright@1.65.0-alpha-2026-10-30 agent-browser@0.41.2 && playwright install --with-deps chromium && agent-browser install --with-deps
RUN mkdir -p /work/browser && chmod 777 /work/browser
WORKDIR /work/browser
