// Names of the networks say bridges to, for places that only have the
// network's key. The server's /networks list is the full source.
const titles: Record<string, string> = {
  discord: "Discord",
  email: "Email",
  irc: "IRC",
  matrix: "Matrix",
  say: "say (home)",
  signal: "Signal",
  slack: "Slack",
  telegram: "Telegram",
  telegram_bot: "Telegram bot",
  webhook: "Webhooks",
};

export function networkTitle(name: string) {
  return titles[name] ?? name;
}
