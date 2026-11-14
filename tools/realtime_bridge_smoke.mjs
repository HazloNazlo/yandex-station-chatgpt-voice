import { spawn } from "node:child_process";
import { createInterface } from "node:readline";

const TIMEOUT_MS = 120_000;
const childEnv = { ...process.env };
// Force the test to use the existing ChatGPT CLI login, never an API key env var.
delete childEnv.OPENAI_API_KEY;

const child = spawn("codex", ["app-server", "--stdio"], {
  cwd: process.cwd(),
  env: childEnv,
  stdio: ["pipe", "pipe", "inherit"],
});

const pending = new Map();
const notificationWaiters = new Map();
let nextId = 1;
let shuttingDown = false;
let threadId;
let realtimeStarted = false;
let nativeHandoffs = 0;
let diagnosticPingCalls = 0;
let speakToUserCalls = 0;
let outputAudioChunks = 0;
let realtimeTranscript = "";

function send(message) {
  if (!child.stdin.writable) throw new Error("Codex App Server stdin is closed");
  child.stdin.write(JSON.stringify(message) + "\n");
}

function rpc(method, params, timeoutMs = 45_000) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error("Timed out waiting for " + method));
    }, timeoutMs);
    pending.set(id, {
      resolve: (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      reject: (error) => {
        clearTimeout(timer);
        reject(error);
      },
    });
    try {
      send({ id, method, params });
    } catch (error) {
      clearTimeout(timer);
      pending.delete(id);
      reject(error);
    }
  });
}

function waitForNotification(method, timeoutMs = 45_000) {
  return new Promise((resolve, reject) => {
    const waiter = {
      resolve,
      reject,
      timer: setTimeout(() => {
        const list = notificationWaiters.get(method) || [];
        notificationWaiters.set(method, list.filter((item) => item !== waiter));
        reject(new Error("Timed out waiting for notification " + method));
      }, timeoutMs),
    };
    const list = notificationWaiters.get(method) || [];
    list.push(waiter);
    notificationWaiters.set(method, list);
  });
}

function publishNotification(method, params) {
  const list = notificationWaiters.get(method);
  if (!list || list.length === 0) return;
  notificationWaiters.delete(method);
  for (const waiter of list) {
    clearTimeout(waiter.timer);
    waiter.resolve(params || {});
  }
}

let resolveToolCycle;
let rejectToolCycle;
const toolCycle = new Promise((resolve, reject) => {
  resolveToolCycle = resolve;
  rejectToolCycle = reject;
});
let toolCycleTimer = setTimeout(
  () => rejectToolCycle(new Error("Timed out waiting for diagnostic_ping + speak_to_user")),
  TIMEOUT_MS,
);

function textResult(text, success = true) {
  return {
    contentItems: [{ type: "inputText", text }],
    success,
  };
}

async function handleServerRequest(message) {
  const params = message.params || {};
  const tool = params.tool;
  const args = params.arguments || {};
  const callId = params.callId || "unknown";

  console.log("TOOL_REQUEST:", tool, JSON.stringify(args));

  try {
    if (tool === "diagnostic_ping") {
      diagnosticPingCalls += 1;
      const value = typeof args.message === "string" ? args.message : "(no message)";
      send({
        id: message.id,
        result: textResult("DIAGNOSTIC_PING_OK: " + value),
      });
      console.log("DIAGNOSTIC_PING_RESULT_SENT");
      return;
    }

    if (tool === "speak_to_user") {
      speakToUserCalls += 1;
      const speech = typeof args.text === "string" ? args.text.trim() : "";
      if (!speech) {
        send({ id: message.id, result: textResult("speak_to_user requires non-empty text", false) });
        return;
      }

      // Speak the delegated result into this same realtime session.
      await rpc("thread/realtime/appendSpeech", {
        threadId,
        text: speech,
      });
      send({ id: message.id, result: textResult("spoken") });
      console.log("SPEECH_APPENDED_TO_REALTIME:", speech);

      if (diagnosticPingCalls >= 1 && speakToUserCalls >= 1) {
        clearTimeout(toolCycleTimer);
        resolveToolCycle();
      }
      return;
    }

    send({
      id: message.id,
      result: textResult("Tool is not allowed in this diagnostic: " + String(tool), false),
    });
  } catch (error) {
    send({
      id: message.id,
      result: textResult("Tool failed: " + String(error?.message || error), false),
    });
    rejectToolCycle(error);
  }
}

const output = createInterface({ input: child.stdout });
output.on("line", (line) => {
  let message;
  try {
    message = JSON.parse(line);
  } catch {
    console.error("Ignoring non-JSON stdout line:", line.slice(0, 200));
    return;
  }

  if (message.method === "item/tool/call" && message.id !== undefined) {
    void handleServerRequest(message);
    return;
  }

  if (message.method === "thread/realtime/started") {
    realtimeStarted = true;
  }

  if (message.method === "thread/realtime/itemAdded") {
    if (message.params?.item?.type === "handoff_request") {
      nativeHandoffs += 1;
      console.log("NATIVE_HANDOFF:", JSON.stringify(message.params.item));
    }
  }

  if (message.method === "thread/realtime/transcript/delta" ||
      message.method === "thread/realtime/transcript/done") {
    const params = message.params || {};
    if (params.role === "user" || params.role === "assistant") {
      const text = params.delta || params.text || "";
      if (text) {
        realtimeTranscript += text;
        console.log("REALTIME_TRANSCRIPT[" + params.role + "]:", text);
      }
    }
  }

  if (message.method === "thread/realtime/outputAudio/delta") {
    outputAudioChunks += 1;
  }

  if (message.method) {
    publishNotification(message.method, message.params);
  }

  if (message.id !== undefined && pending.has(message.id)) {
    const waiter = pending.get(message.id);
    pending.delete(message.id);
    if (message.error) {
      waiter.reject(new Error(message.error.message || JSON.stringify(message.error)));
    } else {
      waiter.resolve(message);
    }
    return;
  }

  if (message.method && message.id !== undefined) {
    send({
      id: message.id,
      error: { code: -32601, message: "Unsupported request in realtime smoke test" },
    });
  }
});

child.once("error", (error) => {
  rejectToolCycle(error);
  for (const waiter of pending.values()) waiter.reject(error);
});

child.once("exit", (code, signal) => {
  if (!shuttingDown) {
    const error = new Error("Codex App Server exited: code=" + code + " signal=" + signal);
    rejectToolCycle(error);
    for (const waiter of pending.values()) waiter.reject(error);
  }
});

async function cleanup() {
  shuttingDown = true;
  clearTimeout(toolCycleTimer);
  for (const waiter of pending.values()) {
    waiter.reject(new Error("Test is shutting down"));
  }
  pending.clear();
  for (const list of notificationWaiters.values()) {
    for (const waiter of list) {
      clearTimeout(waiter.timer);
      waiter.reject(new Error("Test is shutting down"));
    }
  }
  notificationWaiters.clear();

  if (threadId && child.exitCode === null && child.stdin.writable) {
    try {
      await rpc("thread/realtime/stop", { threadId }, 8_000);
    } catch {
      // Continue to process cleanup even if upstream already closed the call.
    }
  }
  if (child.stdin.writable) child.stdin.end();

  if (child.exitCode === null) {
    const killTimer = setTimeout(() => child.kill("SIGTERM"), 2_000);
    killTimer.unref();
    await new Promise((resolve) => child.once("exit", resolve));
    clearTimeout(killTimer);
  }
}

async function main() {
  try {
    const initialize = await rpc("initialize", {
      clientInfo: {
        name: "codex-realtime-diagnostic-ping",
        title: "Codex realtime diagnostic ping",
        version: "0.1.0",
      },
      capabilities: { experimentalApi: true },
    });
    console.log("APP_SERVER_INITIALIZED:", initialize.result?.platformOs || "ok");
    send({ method: "initialized", params: {} });

    const threadResponse = await rpc("thread/start", {
      cwd: process.cwd(),
      model: "gpt-6-luna",
      ephemeral: true,
      approvalPolicy: "never",
      sandbox: "read-only",
      baseInstructions:
        "You are the execution side of one native realtime voice assistant. " +
        "Requests arrive inside a realtime delegation. Use dynamic tools whenever needed. " +
        "For this diagnostic request, call diagnostic_ping exactly once. " +
        "Then call speak_to_user exactly once with a concise statement containing the tool result. " +
        "Do not claim success before the tool result is returned.",
      developerInstructions:
        "Only diagnostic_ping and speak_to_user are available. Do not use shell, browser, or other tools.",
      dynamicTools: [
        {
          type: "function",
          name: "diagnostic_ping",
          description: "Harmless test tool. Returns a fixed diagnostic result for the supplied message.",
          inputSchema: {
            type: "object",
            properties: { message: { type: "string" } },
            required: ["message"],
            additionalProperties: false,
          },
        },
        {
          type: "function",
          name: "speak_to_user",
          description: "Speak the completed delegated result through the active realtime conversation.",
          inputSchema: {
            type: "object",
            properties: { text: { type: "string" } },
            required: ["text"],
            additionalProperties: false,
          },
        },
      ],
    });

    threadId = threadResponse.result?.thread?.id;
    if (!threadId) throw new Error("thread/start returned no thread id");
    console.log("EPHEMERAL_THREAD_STARTED");

    const startedNotification = waitForNotification("thread/realtime/started", 60_000);
    await rpc("thread/realtime/start", {
      threadId,
      outputModality: "audio",
      transport: { type: "websocket" },
      version: "v3",
      voice: "juniper",
      clientManagedHandoffs: false,
      flushTranscriptTailOnSessionEnd: true,
      codexResponsesAsItems: false,
      includeStartupContext: false,
      prompt:
        "You are the native realtime voice surface of one assistant. " +
        "Keep speech natural, concise, and interruptible. " +
        "For requests requiring tools, use the native handoff to the execution agent. " +
        "Do not claim a tool has run until the execution agent returns its result.",
    }, 60_000);
    await startedNotification;
    console.log("REALTIME_SESSION_STARTED:", realtimeStarted ? "yes" : "event received");

    await rpc("thread/realtime/appendText", {
      threadId,
      role: "user",
      text:
        "This is a diagnostic test. Please run diagnostic_ping with the message " +
        "'realtime ping from ChatGPT Free'. Wait for the result and tell me the result.",
    });
    console.log("SIMULATED_USER_UTTERANCE_SENT");

    await toolCycle;
    console.log("NATIVE_HANDOFF_COUNT:", nativeHandoffs);
    console.log("DIAGNOSTIC_PING_CALLS:", diagnosticPingCalls);
    console.log("SPEAK_TO_USER_CALLS:", speakToUserCalls);
    console.log("REALTIME_AUDIO_CHUNKS:", outputAudioChunks);

    if (nativeHandoffs < 1 || diagnosticPingCalls !== 1 || speakToUserCalls !== 1) {
      throw new Error("The complete native realtime handoff cycle was not observed.");
    }

    console.log("PASS: realtime session handed off to diagnostic_ping and accepted spoken result.");
    if (outputAudioChunks > 0) {
      console.log("REALTIME_OUTPUT_AUDIO_OBSERVED: yes");
    } else {
      console.log("REALTIME_OUTPUT_AUDIO_OBSERVED: no chunks observed; appendSpeech was accepted, but audio playback still needs a WebRTC test.");
    }
  } finally {
    await cleanup();
  }
}

main().catch((error) => {
  console.error("FAIL:", error?.message || String(error));
  process.exitCode = 1;
  void cleanup();
});
