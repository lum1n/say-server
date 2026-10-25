// <say-chat> loader, served as /widget.js. It mounts an iframe on
// /embed/$widgetId of the same server, which isolates CSS and auth from the
// host page, and talks to it over postMessage (docs/adr/0012-widget.md).
//
//   <script src="https://chat.example.com/widget.js" async></script>
//   <say-chat widget="pk_…"></say-chat>
//
// Attributes: widget (public key, required), identity (signed JWT from the
// host site), open, inline (fill the element instead of a floating
// launcher). Theme: CSS custom properties named --say-* set on the element
// are forwarded to the iframe; --say-accent also colours the launcher.
// Events: open, close, unread (detail.count).

import { isEmbedMessage, type HostMessage, type Theme } from "@say/sdk/embed";

const origin = new URL((document.currentScript as HTMLScriptElement | null)?.src ?? location.href).origin;

const css = `
:host { all: initial; }
:host([inline]) { display: block; }
:host([inline]) .panel { position: static; width: 100%; height: 100%; display: block; box-shadow: none; border-radius: 0; }
:host([inline]) .launcher { display: none; }
.launcher {
  position: fixed; right: 20px; bottom: 20px; z-index: 2147483646;
  width: 56px; height: 56px; border: 0; border-radius: 50%; cursor: pointer;
  background: var(--say-accent, #2b5fd9); color: #fff;
  box-shadow: 0 4px 16px rgb(0 0 0 / 0.25);
  display: grid; place-items: center;
}
.launcher svg { width: 26px; height: 26px; }
.badge {
  position: absolute; top: -2px; right: -2px; min-width: 20px; height: 20px; padding: 0 5px;
  border-radius: 10px; background: #d92b3a; color: #fff;
  font: 600 12px/20px system-ui, sans-serif; text-align: center;
}
.badge[hidden] { display: none; }
.panel {
  position: fixed; right: 20px; bottom: 88px; z-index: 2147483647;
  width: min(380px, calc(100vw - 40px)); height: min(600px, calc(100vh - 120px));
  border-radius: 12px; overflow: hidden; display: none;
  box-shadow: 0 8px 32px rgb(0 0 0 / 0.3);
}
:host([open]) .panel { display: block; }
@media (max-width: 480px) {
  :host(:not([inline])[open]) .panel { inset: 0; width: 100%; height: 100%; border-radius: 0; }
  :host(:not([inline])[open]) .launcher { display: none; }
}
iframe { border: 0; width: 100%; height: 100%; display: block; }
`;

const chatIcon = `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M4 4h16a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H9l-5 4v-4a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z"/></svg>`;
const closeIcon = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>`;

class SayChat extends HTMLElement {
  static observedAttributes = ["identity", "open"];

  private frame?: HTMLIFrameElement;
  private launcher?: HTMLButtonElement;
  private badge?: HTMLSpanElement;
  private ready = false;

  connectedCallback() {
    const widget = this.getAttribute("widget");
    if (!widget || this.frame) return;
    const root = this.shadowRoot ?? this.attachShadow({ mode: "open" });
    root.innerHTML = `<style>${css}</style>
      <button class="launcher" type="button" part="launcher"><span class="icon"></span><span class="badge" hidden></span></button>
      <div class="panel" part="panel"></div>`;
    this.launcher = root.querySelector<HTMLButtonElement>(".launcher")!;
    this.badge = root.querySelector<HTMLSpanElement>(".badge")!;
    this.launcher.addEventListener("click", () => this.toggle());
    const frame = document.createElement("iframe");
    frame.src = `${origin}/embed/${encodeURIComponent(widget)}`;
    frame.title = "Chat";
    frame.allow = "clipboard-write";
    root.querySelector(".panel")!.append(frame);
    this.frame = frame;
    this.paintLauncher();
    addEventListener("message", this.onMessage);
  }

  disconnectedCallback() {
    removeEventListener("message", this.onMessage);
  }

  attributeChangedCallback(name: string, old: string | null, value: string | null) {
    if (name === "open") this.paintLauncher();
    if (!this.ready || old === value) return;
    if (name === "identity") this.post({ source: "say-host", type: "identity", identity: value });
    if (name === "open") {
      this.post({ source: "say-host", type: value === null ? "close" : "open" });
      this.dispatchEvent(new Event(value === null ? "close" : "open"));
    }
  }

  open() {
    this.setAttribute("open", "");
  }

  close() {
    this.removeAttribute("open");
  }

  toggle() {
    this.toggleAttribute("open");
  }

  /** setTheme forwards the element's --say-* properties again, after they change. */
  setTheme() {
    this.post({ source: "say-host", type: "theme", theme: this.theme() });
  }

  private onMessage = (event: MessageEvent) => {
    if (event.origin !== origin || event.source !== this.frame?.contentWindow) return;
    if (!isEmbedMessage(event.data)) return;
    switch (event.data.type) {
      case "ready":
        this.ready = true;
        this.post({
          source: "say-host",
          type: "init",
          identity: this.getAttribute("identity"),
          theme: this.theme(),
          open: this.isOpen(),
          closable: !this.hasAttribute("inline"),
        });
        break;
      case "unread":
        this.showUnread(event.data.count);
        this.dispatchEvent(new CustomEvent("unread", { detail: { count: event.data.count } }));
        break;
      case "close":
        this.close();
        break;
    }
  };

  private isOpen() {
    return this.hasAttribute("open") || this.hasAttribute("inline");
  }

  private paintLauncher() {
    if (!this.launcher) return;
    const open = this.hasAttribute("open");
    this.launcher.querySelector(".icon")!.innerHTML = open ? closeIcon : chatIcon;
    this.launcher.setAttribute("aria-label", open ? "Close chat" : "Open chat");
    this.launcher.setAttribute("aria-expanded", String(open));
  }

  private showUnread(count: number) {
    if (!this.badge) return;
    this.badge.hidden = count === 0;
    this.badge.textContent = count > 9 ? "9+" : String(count);
  }

  private theme() {
    const style = getComputedStyle(this);
    const theme: Theme = {};
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
