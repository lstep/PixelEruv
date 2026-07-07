// ChatPanel is a DOM sidebar fixed to the right edge of the browser window,
// independent of the Phaser canvas/scene lifecycle. It shows a tabbed
// conversation (Global / Nearby) with a message list and input row.
// Messages are ephemeral — kept in memory only, lost on refresh.
//
// The panel is created once in main.ts and stored on game.registry as
// "chatPanel". GameScene wires WsClient's onChatMessage callback to
// addMessage(). TopMenu toggles visibility via show()/hide().

export type ChatChannel = "global" | "proximity";

export interface ChatMessage {
  channel: ChatChannel;
  entityId: string;
  displayName: string;
  text: string;
  timestamp: number; // unix millis
}

const PANEL_STYLE =
  "position:fixed;top:52px;right:0;width:320px;height:calc(100vh - 52px);background:#1a1a2e;color:#fff;display:none;flex-direction:column;z-index:15;font-family:sans-serif;border-left:1px solid #333;";

export class ChatPanel {
  private container: HTMLDivElement;
  private messagesDiv: HTMLDivElement;
  private input: HTMLInputElement;
  private activeChannel: ChatChannel = "global";
  private messages: Record<ChatChannel, ChatMessage[]> = {
    global: [],
    proximity: [],
  };
  private sendHandler: ((channel: ChatChannel, text: string) => void) | null = null;

  constructor() {
    this.container = document.createElement("div");
    this.container.style.cssText = PANEL_STYLE;
    document.body.appendChild(this.container);

    // Header: tabs + close button.
    const header = document.createElement("div");
    header.style.cssText = "display:flex;align-items:center;padding:8px 12px;border-bottom:1px solid #333;gap:8px;";
    this.container.appendChild(header);

    const tabGlobal = this.makeTab("Global", "global");
    const tabNearby = this.makeTab("Nearby", "proximity");
    header.appendChild(tabGlobal);
    header.appendChild(tabNearby);

    const spacer = document.createElement("div");
    spacer.style.cssText = "flex:1;";
    header.appendChild(spacer);

    const closeBtn = document.createElement("button");
    closeBtn.textContent = "✕";
    closeBtn.style.cssText = "background:none;color:#aaa;border:none;font-size:18px;cursor:pointer;padding:0 4px;";
    closeBtn.addEventListener("click", () => this.hide());
    header.appendChild(closeBtn);

    // Messages list.
    this.messagesDiv = document.createElement("div");
    this.messagesDiv.style.cssText = "flex:1;overflow-y:auto;padding:8px 12px;font-size:13px;line-height:1.4;";
    this.container.appendChild(this.messagesDiv);

    // Input row.
    const inputRow = document.createElement("div");
    inputRow.style.cssText = "display:flex;gap:6px;padding:8px 12px;border-top:1px solid #333;";
    this.container.appendChild(inputRow);

    this.input = document.createElement("input");
    this.input.type = "text";
    this.input.placeholder = `Send to ${this.activeChannel === "global" ? "Global" : "Nearby"}…`;
    this.input.style.cssText =
      "flex:1;padding:8px 10px;font-size:14px;background:#2d2d3a;color:#fff;border:1px solid #555;border-radius:6px;";
    this.input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        this.trySend();
      }
    });
    inputRow.appendChild(this.input);

    const sendBtn = document.createElement("button");
    sendBtn.textContent = "Send";
    sendBtn.style.cssText =
      "padding:8px 14px;font-size:14px;font-weight:600;background:#4c5cf0;color:#fff;border:none;border-radius:6px;cursor:pointer;";
    sendBtn.addEventListener("click", () => this.trySend());
    inputRow.appendChild(sendBtn);

    this.renderTabs();
    this.renderMessages();
  }

  private makeTab(label: string, channel: ChatChannel): HTMLButtonElement {
    const btn = document.createElement("button");
    btn.textContent = label;
    btn.dataset.channel = channel;
    btn.style.cssText =
      "padding:6px 12px;font-size:13px;font-weight:600;background:none;color:#aaa;border:1px solid transparent;border-radius:6px;cursor:pointer;";
    btn.addEventListener("click", () => {
      this.activeChannel = channel;
      this.input.placeholder = `Send to ${label}…`;
      this.renderTabs();
      this.renderMessages();
    });
    return btn;
  }

  private renderTabs(): void {
    const tabs = this.container.querySelectorAll<HTMLButtonElement>("[data-channel]");
    tabs.forEach((tab) => {
      const isActive = tab.dataset.channel === this.activeChannel;
      tab.style.background = isActive ? "#2d2d3a" : "none";
      tab.style.color = isActive ? "#fff" : "#aaa";
      tab.style.borderColor = isActive ? "#4c5cf0" : "transparent";
    });
  }

  // addMessage appends a chat message to the right channel's history and
  // re-renders if that tab is active. Called by GameScene from
  // WsClient.onChatMessage.
  addMessage(msg: ChatMessage): void {
    const ch = (msg.channel === "global" || msg.channel === "proximity") ? msg.channel : "global";
    this.messages[ch].push(msg);
    if (ch === this.activeChannel) {
      this.appendMessageEl(msg);
      this.scrollToBottom();
    }
  }

  // setSendHandler wires the send callback. Called by GameScene once the
  // WsClient is available.
  setSendHandler(fn: (channel: ChatChannel, text: string) => void): void {
    this.sendHandler = fn;
  }

  show(): void {
    this.container.style.display = "flex";
    this.scrollToBottom();
    this.input.focus();
  }

  hide(): void {
    this.container.style.display = "none";
  }

  isVisible(): boolean {
    return this.container.style.display === "flex";
  }

  toggle(): void {
    if (this.isVisible()) this.hide();
    else this.show();
  }

  private trySend(): void {
    const text = this.input.value.trim();
    if (!text || !this.sendHandler) return;
    this.sendHandler(this.activeChannel, text);
    this.input.value = "";
  }

  private renderMessages(): void {
    this.messagesDiv.innerHTML = "";
    for (const msg of this.messages[this.activeChannel]) {
      this.appendMessageEl(msg);
    }
    this.scrollToBottom();
  }

  private appendMessageEl(msg: ChatMessage): void {
    const el = document.createElement("div");
    el.style.cssText = "margin-bottom:10px;";

    const time = new Date(msg.timestamp);
    const hh = String(time.getHours()).padStart(2, "0");
    const mm = String(time.getMinutes()).padStart(2, "0");

    const head = document.createElement("div");
    head.style.cssText = "display:flex;gap:6px;align-items:baseline;";
    const name = document.createElement("b");
    name.textContent = msg.displayName;
    name.style.cssText = "color:#4c9cf0;";
    const ts = document.createElement("span");
    ts.textContent = `${hh}:${mm}`;
    ts.style.cssText = "color:#666;font-size:11px;";
    head.appendChild(name);
    head.appendChild(ts);
    el.appendChild(head);

    const body = document.createElement("div");
    body.textContent = msg.text;
    body.style.cssText = "word-wrap:break-word;white-space:pre-wrap;";
    el.appendChild(body);

    this.messagesDiv.appendChild(el);
  }

  private scrollToBottom(): void {
    // Only auto-scroll if the user is near the bottom (within 50px). If they
    // scrolled up to read history, don't yank them down on a new message.
    const div = this.messagesDiv;
    if (div.scrollTop + div.clientHeight >= div.scrollHeight - 50) {
      div.scrollTop = div.scrollHeight;
    }
  }
}
