// Static site configuration. URLs and external links live here, never baked
// into translation strings, so marketing can change them without touching copy.
export const SITE = {
  baseUrl: "https://flock.duckbug.io",
  managedUrl: "https://roost.duckbug.io",
  githubUrl: "https://github.com/duckbugio/flock",
  githubIssuesUrl: "https://github.com/duckbugio/flock/issues",
  docsUrl: "https://github.com/duckbugio/flock#readme",
  licenseUrl: "https://github.com/duckbugio/flock/blob/main/LICENSE",
  dockerImage: "ghcr.io/duckbugio/flock-telegram",
  ogImage: "/og-image.png",
} as const;

// The quickstart code block. Kept out of translations so the commands stay
// identical across every locale.
export const QUICKSTART_SNIPPET = `docker pull ghcr.io/duckbugio/flock-telegram:latest

# .env
TELEGRAM_BOT_TOKEN=your-bot-token
CLAUDE_CODE_OAUTH_TOKEN=your-claude-token

docker compose up -d`;
