# Free-plan test for the Codex App Server bridge

**Purpose:** establish whether this ChatGPT Free account can use the official Codex CLI and App Server path before spending time on a realtime voice integration. This test does not require an OpenAI Developer API key, does not change the Station, and does not expose credentials.

**Run on the user's Mac**, not on Yandex Station Mini 1. The Mac is the first control environment because it has a browser for ChatGPT sign-in and a supported local Codex CLI environment.

## Important plan distinction

OpenAI currently describes Codex as included on Free, but its Free-plan details list GPT-6 Luna in the ChatGPT desktop app. The Plus plan explicitly lists CLI, IDE, and web access. Codex App Server is a CLI component and is currently marked Experimental. Free access to the exact App Server surface used by this bridge is therefore not documented as available.

The separate official Sign in with ChatGPT plan-sharing flow for third-party open-source apps currently documents Plus/Pro eligibility, not Free. Do not assume that a Free user's desktop-app access is automatically available to an external app.

References:
- https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan
- https://learn.chatgpt.com/docs/pricing
- https://learn.chatgpt.com/docs/models
- https://learn.chatgpt.com/docs/cli/reference
- https://developers.openai.com/siwc/quickstart

## Gate 1 — install and sign in with the official CLI

Open Terminal on the Mac.

Check prerequisites:

```bash
node --version
npm --version
```

Install the current CLI release as of 14 November 2026:

```bash
npm install --global @openai/codex@0.169.0
codex --version
```

Start the official ChatGPT login flow:

```bash
codex login
```

Complete the browser sign-in with the same Free account used for ChatGPT. Then check the auth mode:

```bash
codex login status
```

Expected: logged in with ChatGPT. If it reports API key auth, do not continue; sign out with `codex logout` and repeat using ChatGPT login.

## Gate 2 — make one tiny read-only model request

Try the Free-listed model:

```bash
codex exec --model gpt-6-luna --sandbox read-only --skip-git-repo-check \
  "Reply with exactly CODEX_FREE_OK"
```

Expected success: the final response is exactly `CODEX_FREE_OK` (some progress/status text may also be printed).

Interpretation:

- If it succeeds, the account can run at least one Codex CLI inference. This is a necessary gate, not proof that the realtime voice feature is enabled.
- If it fails with an account/plan/model entitlement error, stop here. The Free desktop-app allowance is not sufficient evidence for CLI/App Server use. Do not try to bypass the gate using another OAuth client ID or copied private tokens.
- If it fails for installation, DNS, TLS, or network reasons, resolve that separately; it does not prove plan ineligibility.
- If the CLI says the model is unavailable, do not randomly substitute a paid-tier model. Check the model picker in the Free desktop app and report the exact error.

## Gate 3 — confirm App Server presence

Only do this if Gate 2 succeeded.

```bash
codex app-server --help
codex features list
```

The current CLI reference marks `codex app-server` Experimental. The third-party realtime implementation additionally expects the private `realtime_conversation` feature and methods such as `thread/realtime/start`. Their presence in a CLI build is not a guarantee that OpenAI has enabled the feature for this account.

Do not enable undocumented flags or edit the user's normal Codex config yet. Capture the output and inspect whether `realtime_conversation` is listed.

## Gate 4 — native handoff and diagnostic_ping

This gate requires a separate local/temporary app-server harness using the pinned third-party reference implementation. It should be built only after Gates 1–3 pass. The harness will:

1. Start a Codex App Server child process with a disposable CODEX_HOME.
2. Start one temporary realtime session in the reference browser client (not the Station).
3. Register one harmless server-side tool named `diagnostic_ping`.
4. Ask the voice model to run the ping.
5. Verify all four signals: native `handoff_request`, `item/tool/call` for `diagnostic_ping`, the JSON result, and audible `speak_to_user` output.
6. Close the realtime session and confirm the child process and temporary credentials are removed.

This is not yet a supported OpenAI public voice API; the realtime extension is experimental/private. Do not connect the Station or use real tools until this isolated test works.

## What to send back

Paste only the following non-secret outputs:

- `codex --version`
- `codex login status` (redact account email/name if displayed)
- Whether Gate 2 succeeded, or the exact error text
- Whether `realtime_conversation` appears in `codex features list`

**Never send** `~/.codex/auth.json`, access tokens, refresh tokens, browser cookies, OAuth callback URLs, or screenshots that expose them.

## Stop conditions

Stop immediately and do not buy a paid plan or alter the Station if:

- ChatGPT login cannot be completed,
- the CLI rejects the Free account for model inference,
- App Server is unavailable in the installed CLI,
- the realtime feature is missing or the bridge reports an entitlement/protocol error.

At that point the next decision is whether to stay with the current browser-session path and improve the local tool gateway, or evaluate a paid Codex plan. Do not silently fall back to API billing.
