import { afterEach, describe, expect, it, vi } from "vitest";
import { mailApi, type MessageTranslation } from "./mail";

function streamOf(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
}

function stubStream(chunks: string[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({ ok: true, status: 200, body: streamOf(chunks) })),
  );
}

async function run() {
  const results: MessageTranslation[] = [];
  const errors: string[] = [];
  await mailApi.translateMessageStream(
    "tenant-1",
    "account-1",
    "msg-1",
    { folder: "inbox", id_mode: "uid" },
    { onResult: (r) => results.push(r), onError: (m) => errors.push(m) },
    new AbortController().signal,
  );
  return { results, errors };
}

afterEach(() => vi.unstubAllGlobals());

describe("translateMessageStream", () => {
  it("先收到快引擎结果,再收到大模型的替换结果", async () => {
    stubStream([
      'id: 1\nevent: result\ndata: {"text":"旧","engine":"libretranslate","truncated":false,"cached":false,"final":false}\n\n',
      'id: 2\nevent: result\ndata: {"text":"新","engine":"llm","truncated":false,"cached":false,"final":true}\n\n',
    ]);
    const { results, errors } = await run();
    expect(results.map((r) => r.text)).toEqual(["旧", "新"]);
    expect(results[1].final).toBe(true);
    expect(errors).toEqual([]);
  });

  it("一帧跨多个数据块时仍能解析", async () => {
    const frame =
      'event: result\ndata: {"text":"跨块","engine":"llm","truncated":false,"cached":false,"final":true}\n\n';
    stubStream([frame.slice(0, 20), frame.slice(20)]);
    const { results } = await run();
    expect(results).toHaveLength(1);
    expect(results[0].text).toBe("跨块");
  });

  it("引擎全失败时按错误帧回调", async () => {
    stubStream([
      'event: error\ndata: {"code":1006,"message":"翻译服务暂时不可用,请稍后重试"}\n\n',
    ]);
    const { results, errors } = await run();
    expect(results).toEqual([]);
    expect(errors).toEqual(["翻译服务暂时不可用,请稍后重试"]);
  });
});