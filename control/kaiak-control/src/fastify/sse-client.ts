// A small server-sent-events client for the plugin's tests: reads a real HTTP response
// body line by line as the SSE format defines it (fields, comments, blank-line
// dispatch) and hands out what arrives in order.

export type SseItem =
  | { kind: "event"; event: string; data: string; id?: string }
  | { kind: "comment"; text: string }
  | { kind: "end" };

export interface SseStream {
  response: Response;
  // The next event or comment, or end once the server has closed the stream.
  next(): Promise<SseItem>;
  // The next event, skipping comments.
  nextEvent(): Promise<Exclude<SseItem, { kind: "comment" }>>;
  // Drops the connection, as a gateway going away does.
  close(): void;
}

export async function openSseStream(url: string, headers: Record<string, string>): Promise<SseStream> {
  const abort = new AbortController();
  const response = await fetch(url, { headers, signal: abort.signal });
  const items: SseItem[] = [];
  const waiters: ((item: SseItem) => void)[] = [];

  const deliver = (item: SseItem): void => {
    const waiter = waiters.shift();
    if (waiter) waiter(item);
    else items.push(item);
  };

  const readBody = async (body: ReadableStream<Uint8Array>): Promise<void> => {
    const decoder = new TextDecoder();
    let buffer = "";
    let event: { event?: string; data: string[]; id?: string } = { data: [] };
    try {
      for await (const bytes of body) {
        buffer += decoder.decode(bytes, { stream: true });
        let newline = buffer.indexOf("\n");
        while (newline !== -1) {
          const line = buffer.slice(0, newline);
          buffer = buffer.slice(newline + 1);
          newline = buffer.indexOf("\n");
          if (line === "") {
            if (event.data.length > 0 || event.event !== undefined) {
              const item: SseItem = { kind: "event", event: event.event ?? "message", data: event.data.join("\n") };
              deliver(event.id === undefined ? item : { ...item, id: event.id });
            }
            event = { data: [] };
            continue;
          }
          if (line.startsWith(":")) {
            deliver({ kind: "comment", text: line.slice(1).trim() });
            continue;
          }
          const colon = line.indexOf(":");
          const field = colon === -1 ? line : line.slice(0, colon);
          const value = colon === -1 ? "" : line.slice(colon + 1).replace(/^ /, "");
          if (field === "event") event.event = value;
          else if (field === "data") event.data.push(value);
          else if (field === "id") event.id = value;
        }
      }
    } catch (error) {
      // Closing the stream from this side aborts the read; that is its end.
      if (!abort.signal.aborted) throw error;
    }
    deliver({ kind: "end" });
  };

  if (response.body) {
    const body = response.body;
    void readBody(body);
  } else {
    deliver({ kind: "end" });
  }

  const next = (): Promise<SseItem> => {
    const item = items.shift();
    if (item) return Promise.resolve(item);
    return new Promise((resolve) => waiters.push(resolve));
  };

  return {
    response,
    next,
    async nextEvent() {
      for (;;) {
        const item = await next();
        if (item.kind !== "comment") return item;
      }
    },
    close() {
      abort.abort();
    },
  };
}
