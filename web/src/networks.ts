// Names of the networks say bridges to, for places that only have the
// network's key. The server's /networks list is the full source.
const titles: Record<string, string> = {
  email: "Email",
  irc: "IRC",
  matrix: "Matrix",
  signal: "Signal",
  slack: "Slack",
  telegram: "Telegram",
};

export function networkTitle(name: string) {
  return titles[name] ?? name;
}
