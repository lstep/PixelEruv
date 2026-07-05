import { create, toBinary, fromBinary } from "@bufbuild/protobuf";
import { context, trace } from "@opentelemetry/api";
import { ClientFrameSchema, ServerFrameSchema, AuthFrameSchema, InputFrameSchema, InputStateSchema } from "../proto/frames_pb";
import { PositionSchema } from "../proto/components_pb";
import { tracer, traceparentFor } from "../otel";
import { getIdToken } from "../auth";

export type ReplicationHandler = (batch: ReplicationBatchView) => void;

export interface SpawnEntityView {
  entityId: string;
  components: { componentId: number; data: Uint8Array }[];
}

export interface UpdateComponentView {
  entityId: string;
  componentId: number;
  data: Uint8Array;
}

export interface DestroyEntityView {
  entityId: string;
}

export interface ReplicationBatchView {
  lastInputSeq: number;
  spawns: SpawnEntityView[];
  updates: UpdateComponentView[];
  destroys: DestroyEntityView[];
}

export class WsClient {
  private ws: WebSocket | null = null;
  private url: string;
  private clientId: string | null = null;
  private seq = 0;

  constructor(url: string) {
    this.url = url;
  }

  connect(onReady: () => void, onReplication: ReplicationHandler): void {
    const connectSpan = tracer.startSpan("ws.connect");
    this.ws = new WebSocket(this.url);
    this.ws.binaryType = "arraybuffer";

    this.ws.onopen = () => {
      const authSpan = tracer.startSpan("ws.send_auth");
      try {
        // Build the auth frame inside the span's active context so
        // traceparentFor() serializes this span's context for the backend.
        const auth = context.with(trace.setSpan(context.active(), authSpan), () =>
          create(AuthFrameSchema, { idToken: getIdToken() ?? "dev", traceparent: traceparentFor() }),
        );
        const frame = create(ClientFrameSchema, { payload: { case: "auth", value: auth } });
        this.ws!.send(toBinary(ClientFrameSchema, frame));
      } finally {
        authSpan.end();
      }
    };

    this.ws.onmessage = (event) => {
      const data = new Uint8Array(event.data as ArrayBuffer);
      const serverFrame = fromBinary(ServerFrameSchema, data);

      switch (serverFrame.payload.case) {
        case "authResult": {
          const ar = serverFrame.payload.value;
          if (ar.ok) {
            this.clientId = ar.clientId;
            connectSpan.setAttribute("client.id", ar.clientId);
            connectSpan.end();
            console.log(`authenticated: client=${this.clientId}`);
            onReady();
          } else {
            connectSpan.recordException(new Error("auth failed"));
            connectSpan.setStatus({ code: 2, message: "auth failed" });
            connectSpan.end();
            console.error("auth failed");
          }
          break;
        }
        case "replication": {
          const batch = serverFrame.payload.value;
          const span = tracer.startSpan("ws.on_replication", {
            attributes: {
              "client.id": this.clientId ?? "",
              "batch.spawns": batch.spawns.length,
              "batch.updates": batch.updates.length,
              "batch.destroys": batch.destroys.length,
              "batch.last_input_seq": Number(batch.lastInputSeq),
            },
          });
          try {
            onReplication({
              lastInputSeq: Number(batch.lastInputSeq),
              spawns: batch.spawns.map((s: any) => ({
                entityId: s.entityId,
                components: s.components.map((c: any) => ({ componentId: c.componentId, data: c.data })),
              })),
              updates: batch.updates.map((u: any) => ({
                entityId: u.entityId,
                componentId: u.componentId,
                data: u.data,
              })),
              destroys: batch.destroys.map((d: any) => ({
                entityId: d.entityId,
              })),
            });
          } finally {
            span.end();
          }
          break;
        }
        case "pong":
          break;
        case "error":
          console.error("server error:", serverFrame.payload.value);
          break;
      }
    };

    this.ws.onclose = () => {
      const span = tracer.startSpan("ws.close");
      span.setAttribute("client.id", this.clientId ?? "");
      span.end();
      console.log("websocket closed");
    };
    this.ws.onerror = (err) => {
      connectSpan.recordException(new Error("websocket error"));
      connectSpan.setStatus({ code: 2, message: "websocket error" });
      console.error("websocket error:", err);
    };
  }

  sendInput(state: { up: boolean; down: boolean; left: boolean; right: boolean; run: boolean }): number {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return 0;
    this.seq++;
    const span = tracer.startSpan("ws.send_input", {
      attributes: {
        "client.id": this.clientId ?? "",
        "input.seq": this.seq,
        "input.up": state.up,
        "input.down": state.down,
        "input.left": state.left,
        "input.right": state.right,
        "input.run": state.run,
      },
    });
    try {
      // Serialize this span's context into the frame so the backend's
      // input-handling spans parent to ws.send_input.
      const inputState = create(InputStateSchema, state);
      const input = context.with(trace.setSpan(context.active(), span), () =>
        create(InputFrameSchema, {
          seq: this.seq,
          clientTick: 0,
          state: inputState,
          traceparent: traceparentFor(),
        }),
      );
      const frame = create(ClientFrameSchema, { payload: { case: "input", value: input } });
      this.ws.send(toBinary(ClientFrameSchema, frame));
    } finally {
      span.end();
    }
    return this.seq;
  }

  getClientId(): string | null {
    return this.clientId;
  }

  close(): void {
    this.ws?.close();
  }
}

export function decodePosition(data: Uint8Array): { x: number; y: number; mapId: string; dir: number } {
  const pos = fromBinary(PositionSchema, data);
  return { x: pos.x, y: pos.y, mapId: pos.mapId, dir: pos.dir };
}
