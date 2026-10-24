// <say-chat> loader, served as /widget.js. It mounts an iframe on
// /embed/$widgetId of the same server, which isolates CSS and auth from the
// host page, and talks to it over postMessage.
//
//   <script src="https://chat.example.com/widget.js" async></script>
//   <say-chat widget="pk_…"></say-chat>
//
// Attributes: widget (publishable key, required), identity (signed JWT from
// the host site), open. Theme: CSS custom properties named --say-* set on the
// element are forwarded to the iframe.

import { isEmbedMessage, type HostMessage } from "@say/sdk/embed";

const origin = new URL((document.currentScript as HTMLScriptElement | null)?.src ?? location.href).origin;

class SayChat extends HTMLElement {
  static observedAttributes = ["identity", "open"];

  private frame?: HTMLIFrameElement;
  private ready = false;

  connectedCallback() {
    const widget = this.getAttribute("widget");
    if (!widget || this.frame) return;
    const frame = document.createElement("iframe");
    frame.src = `${origin}/embed/${encodeURIComponent(widget)}`;
    frame.title = "Chat";
    frame.style.cssText = "border:0;width:100%;height:100%";
    this.attachShadow({ mode: "open" }).append(frame);
    this.frame = frame;
    addEventListener("message", this.onMessage);
  }

  disconnectedCallback() {
    removeEventListener("message", this.onMessage);
  }

  attributeChangedCallback() {
    if (this.ready) this.sync();
  }

  open() {
    this.setAttribute("open", "");
  }

  close() {
    this.removeAttribute("open");
  }

  private onMessage = (event: MessageEvent) => {
    if (event.origin !== origin || event.source !== this.frame?.contentWindow) return;
    if (!isEmbedMessage(event.data)) return;
    if (event.data.type === "ready") {
      this.ready = true;
      this.sync();
    } else if (event.data.type === "unread") {
      this.dispatchEvent(new CustomEvent("unread", { detail: { count: event.data.count } }));
    }
  };

  private sync() {
    const identity = this.getAttribute("identity");
    if (identity) this.post({ source: "say-host", type: "identity", token: identity });
    this.post({ source: "say-host", type: "theme", theme: this.theme() });
    this.post({ source: "say-host", type: this.hasAttribute("open") ? "open" : "close" });
  }

  private theme() {
    const style = getComputedStyle(this);
    const theme: Record<string, string> = {};
    for (const name of Array.from(style)) {
      if (name.startsWith("--say-")) theme[name] = style.getPropertyValue(name).trim();
    }
    return theme;
  }

  private post(message: HostMessage) {
    this.frame?.contentWindow?.postMessage(message, origin);
  }
}

if (!customElements.get("say-chat")) customElements.define("say-chat", SayChat);
