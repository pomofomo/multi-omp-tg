// TRD (Telegram Repo Dispatcher) omp extension.
//
// Bridges omp to the dispatcher's localhost HTTP control plane so the
// agent can react to Telegram messages without ever holding the bot
// token. The dispatcher injects per-run env vars before spawning omp:
//
//   TRD_CHAT_ID         numeric Telegram chat id
//   TRD_MESSAGE_ID      numeric id of the message that triggered this run
//   TRD_INSTANCE_ID     this run's instance UUID — used to authorise
//                       the /api/restart endpoint (only the controller
//                       instance is allowed to trigger a re-exec).
//   TRD_DISPATCHER_URL  e.g. http://127.0.0.1:7777
//
// Two-mark visibility convention:
//
//   👀  the dispatcher sets this BEFORE spawning omp (see enqueueOrRun)
//       to signal "system got the message". The extension is not
//       involved.
//
//   👍  the LLM sets this via the tg_react tool registered below, as
//       the first action of its turn, to signal "the model has seen
//       the message". The ACKNOWLEDGE pattern in --append-system-prompt
//       instructs the agent to do so.
//
// Later in the turn the agent MAY call tg_react again with a different
// emoji to reflect outcome (🎉 success, 😅 soft failure, ❌ hard
// failure).
//
// This file is embedded in the Go binary (see extension.go) and written
// to ~/.trd/ext/tg.ts on dispatcher startup. omp is invoked with
// `--extension <path>` per run.

import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

type Env = {
	chatID: string;
	messageID: string;
	dispatcherURL: string;
	instanceID: string;
};

function readEnv(): Env | null {
	const chatID = process.env.TRD_CHAT_ID;
	const messageID = process.env.TRD_MESSAGE_ID;
	const dispatcherURL = process.env.TRD_DISPATCHER_URL;
	const instanceID = process.env.TRD_INSTANCE_ID;
	if (!chatID || !messageID || !dispatcherURL || !instanceID) {
		return null;
	}
	return { chatID, messageID, dispatcherURL, instanceID };
}

async function postReact(env: Env, emoji: string, signal?: AbortSignal): Promise<Response> {
	return fetch(`${env.dispatcherURL}/api/tg/react`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({
			chat_id: Number(env.chatID),
			message_id: Number(env.messageID),
			emoji,
		}),
		signal,
	});
}

export default function trdExtension(pi: ExtensionAPI) {
	const z = pi.zod;
	const env = readEnv();

	pi.registerTool({
		name: "tg_react",
		label: "Telegram Reaction",
		description:
			"Set or replace the emoji reaction on the user's current Telegram " +
			"message. Call this once at the start of every turn with \"👍\" to " +
			"signal that you have seen the message (the dispatcher's own 👀 " +
			"only signals that the system received it, not that the model has " +
			"read it). Later you MAY call this tool again with a different " +
			"emoji to reflect the outcome (e.g. 🎉 success, 😅 soft failure, " +
			"❌ hard failure). Argument: emoji (a single Telegram-supported " +
			"emoji).",
		parameters: z.object({
			emoji: z.string().describe("Single emoji, e.g. 👍"),
		}),
		async execute(_toolCallId, params, _onUpdate, _ctx, signal) {
			if (!env) {
				return {
					content: [
						{
							type: "text",
							text: "tg_react: dispatcher env not configured (TRD_CHAT_ID/TRD_MESSAGE_ID/TRD_DISPATCHER_URL missing)",
						},
					],
					details: { ok: false, reason: "no-env" },
				};
			}
			const emoji = (params.emoji ?? "").trim();
			if (!emoji) {
				return {
					content: [{ type: "text", text: "tg_react: emoji is required" }],
					details: { ok: false, reason: "no-emoji" },
				};
			}
			try {
				const resp = await postReact(env, emoji, signal);
				if (!resp.ok) {
					const body = await resp.text().catch(() => "");
					return {
						content: [
							{
								type: "text",
								text: `tg_react: dispatcher returned HTTP ${resp.status} ${body}`.trim(),
							},
						],
						details: { ok: false, status: resp.status },
					};
				}
				return {
					content: [{ type: "text", text: `reacted with ${emoji}` }],
					details: { ok: true, emoji },
				};
			} catch (err) {
				return {
					content: [{ type: "text", text: `tg_react: ${String(err)}` }],
					details: { ok: false, reason: "fetch-failed" },
				};
			}
		},
	});

	pi.registerTool({
		name: "trd_restart",
		label: "TRD Dispatcher Restart",
		description:
			"Trigger a graceful in-place restart of the TRD dispatcher. " +
			"Drains every in-flight agent run, persists deferred prompts, " +
			"then re-execs the dispatcher binary so live code updates are " +
			"picked up without losing in-flight replies. ONLY callable from " +
			"the controller instance (set via `trd promote <name>` on the " +
			"host); other instances get HTTP 403 from the dispatcher. The " +
			"currently-running turn is allowed to finish — call this once " +
			"you have already sent any user-facing reply for the request.",
		parameters: z.object({}),
		async execute(_toolCallId, _params, _onUpdate, _ctx, signal) {
			if (!env) {
				return {
					content: [
						{
							type: "text",
							text: "trd_restart: dispatcher env not configured (TRD_INSTANCE_ID/TRD_DISPATCHER_URL missing)",
						},
					],
					details: { ok: false, reason: "no-env" },
				};
			}
			try {
				const resp = await fetch(`${env.dispatcherURL}/api/restart`, {
					method: "POST",
					headers: {
						"X-Trd-Instance": env.instanceID,
					},
					signal,
				});
				const body = await resp.text().catch(() => "");
				if (resp.status === 403) {
					return {
						content: [
							{
								type: "text",
								text:
									"trd_restart: not authorised. This instance is not the controller. " +
									"Run `trd promote <repo-name>` on the host to enable.",
							},
						],
						details: { ok: false, status: 403 },
					};
				}
				if (!resp.ok) {
					return {
						content: [
							{
								type: "text",
								text: `trd_restart: HTTP ${resp.status} ${body}`.trim(),
							},
						],
						details: { ok: false, status: resp.status },
					};
				}
				return {
					content: [
						{
							type: "text",
							text:
								"trd_restart: accepted. Dispatcher will drain in-flight " +
								"runs (including this one) and re-exec in place.",
						},
					],
					details: { ok: true, status: resp.status },
				};
			} catch (err) {
				return {
					content: [{ type: "text", text: `trd_restart: ${String(err)}` }],
					details: { ok: false, reason: "fetch-failed" },
				};
			}
		},
	});
}
