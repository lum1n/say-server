// A bot that echoes what it is asked, streaming the answer word by word.
//
//   SAY_URL=http://localhost:7290 SAY_TOKEN=say_… node bot.ts
//
// Create the agent on the People page (or POST /api/v1/principals), add it
// to a room, and mention it: "@Echo hello there".
import { Bot } from "@say/sdk";

const url = process.env.SAY_URL ?? "http://localhost:7290";
const token = process.env.SAY_TOKEN;
if (!token) {
  console.error("Set SAY_TOKEN to the agent's access token.");
  process.exit(1);
}

const bot = new Bot({ url, token, name: "echo-bot", version: "0.1.0", capabilities: ["streaming"] });

bot.on("message", async (msg, ctx) => {
  const words = msg.text.replace(new RegExp(`@${bot.me?.display_name}\\b`, "gi"), "").trim().split(/\s+/);
  const reply = await ctx.stream();
  for (const word of words.filter(Boolean)) {
    reply.append(`${word} `);
    await new Promise((r) => setTimeout(r, 100));
  }
  await reply.end(words.join(" ") || "(nothing to echo)");
});

await bot.start();
console.log(`${bot.me?.display_name} is online at ${url}`);
process.on("SIGINT", () => {
  bot.stop();
  process.exit(0);
});
