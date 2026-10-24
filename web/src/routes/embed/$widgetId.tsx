import { createFileRoute } from "@tanstack/react-router";
import { useEffect } from "react";
import { isHostMessage, type EmbedMessage } from "@say/sdk/embed";

// Mounted inside the iframe that <say-chat> creates on a host page.
export const Route = createFileRoute("/embed/$widgetId")({
  component: Embed,
});

function Embed() {
  const { widgetId } = Route.useParams();

  useEffect(() => {
    const onMessage = (event: MessageEvent) => {
      // The allowed-origins check happens on the server when the widget
      // session is created; here we only accept well-formed messages.
      if (!isHostMessage(event.data)) return;
      if (event.data.type === "theme") {
        for (const [name, value] of Object.entries(event.data.theme)) {
          document.documentElement.style.setProperty(name, value);
        }
      }
    };
    window.addEventListener("message", onMessage);
    const ready: EmbedMessage = { source: "say-embed", type: "ready" };
    window.parent.postMessage(ready, "*");
    return () => window.removeEventListener("message", onMessage);
  }, []);

  return (
    <main>
      <p>Widget {widgetId}</p>
    </main>
  );
}
