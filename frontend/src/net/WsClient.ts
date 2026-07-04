import { create, toBinary, fromBinary } from "@bufbuild/protobuf";
import { ClientFrameSchema, ServerFrameSchema, AuthFrameSchema, InputFrameSchema, InputStateSchema } from "../proto/frames_pb";
import { PositionSchema } from "../proto/components_pb";

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
    this.ws = new WebSocket(this.url);
    this.ws.binaryType = "arraybuffer";

    this.ws.onopen = () => {
      const auth = create(AuthFrameSchema, { idToken: "dev" });
      const frame = create(ClientFrameSchema, { payload: { case: "auth", value: auth } });
      this.ws!.send(toBinary(ClientFrameSchema, frame));
    };

    this.ws.onmessage = (event) => {
      const data = new Uint8Array(event.data as ArrayBuffer);
      const serverFrame = fromBinary(ServerFrameSchema, data);

      switch (serverFrame.payload.case) {
        case "authResult": {
          const ar = serverFrame.payload.value;
          if (ar.ok) {
            this.clientId = ar.clientId;
            console.log(`authenticated: client=${this.clientId}`);
            onReady();
          } else {
            console.error("auth failed");
          }
          break;
        }
        case "replication": {
          const batch = serverFrame.payload.value;
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
          break;
        }
        case "pong":
          break;
        case "error":
          console.error("server error:", serverFrame.payload.value);
          break;
      }
    };

    this.ws.onclose = () => console.log("websocket closed");
    this.ws.onerror = (err) => console.error("websocket error:", err);
  }

  sendInput(state: { up: boolean; down: boolean; left: boolean; right: boolean; run: boolean }): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    this.seq++;
    const inputState = create(InputStateSchema, state);
    const input = create(InputFrameSchema, { seq: this.seq, clientTick: 0, state: inputState });
    const frame = create(ClientFrameSchema, { payload: { case: "input", value: input } });
    this.ws.send(toBinary(ClientFrameSchema, frame));
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
