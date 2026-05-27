// TRD (Telegram Repo Dispatcher) omp extension.
//
// Bridges omp to the dispatcher's localhost HTTP control plane so the
// agent can react to Telegram messages without ever holding the bot
// token. The dispatcher injects per-run env vars before spawning omp:
//
//   TRD_CHAT_ID         numeric Telegram chat id
//   TRD_MESSAGE_ID      numeric id of the message that triggered this run
//   TRD_DISPATCHER_URL  e.g. http://127.0.0.1:7777
//   TRD_ACK_EMOJI       optional override for the auto-acknowledgement
//                       reaction (default "👍")
//
// Behaviour:
//
//   - On `agent_start` we POST a 👍 reaction automatically. omp -p emits
//     this event exactly once per invocation, so the user gets a
//     deterministic "got it" the moment the agent starts thinking — no
//     reliance on the LLM choosing to call a tool.
//
//   - The `tg_react` tool stays registered so the agent can still set
//     other reactions later in the turn (e.g. 🎉 on a success, 😅 on a
//     soft error). Self-driven calls now layer on top of the automatic
//     ack instead of substituting for it.
//
// This file is embedded in the Go binary (see extension.go) and written
// to ~/.trd/ext/tg.ts on dispatcher startup. omp is invoked with
// `--extension <path>` per run.

import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

const DEFAULT_ACK_EMOJI = "👍";

type Env = {
	chatID: string;
	messageID: string;
	dispatcherURL: string;
};

function readEnv(): Env | null {
	const chatID = process.env.TRD_CHAT_ID;
	const messageID = process.env.TRD_MESSAGE_ID;
	const dispatcherURL = process.env.TRD_DISPATCHER_URL;
	if (!chatID || !messageID || !dispatcherURL) {
		return null;
	}
	return { chatID, messageID, dispatcherURL };
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

	// --- Automatic acknowledgement on agent_start ---------------------
	//
	// Fires once per `omp -p` invocation. Best-effort: a failed POST is
	// logged and swallowed — we never block the agent on a Telegram
	// reaction.
	pi.on("agent_start", async () => {
		if (!env) {
			pi.logger?.debug?.("trd-ext: skipping auto-react, dispatcher env missing");
			return;
		}
		const emoji = process.env.TRD_ACK_EMOJI || DEFAULT_ACK_EMOJI;
		try {
			const resp = await postReact(env, emoji);
			if (!resp.ok) {
				const body = await resp.text().catch(() => "");
				pi.logger?.warn?.("trd-ext: auto-react failed", {
					status: resp.status,
					body,
				});
			}
		} catch (err) {
			pi.logger?.warn?.("trd-ext: auto-react errored", { err: String(err) });
		}
	});

	// --- Self-driven tg_react tool ------------------------------------
	//
	// Lets the agent set additional reactions later in the turn (e.g.
	// swap 👍 → 🎉 on success, or → 😅 on a soft failure). The automatic
	// agent_start hook already covers the "I got your message" signal.
	pi.registerTool({
		name: "tg_react",
		label: "Telegram Reaction",
		description:
			"Set or replace the emoji reaction on the user's current Telegram " +
			"message. A 👍 acknowledgement is already sent automatically when " +
			"the turn starts; use this tool only when a different emoji better " +
			"reflects the outcome (e.g. 🎉 on success, 😅 on a soft failure). " +
			"Argument: emoji (a single Telegram-supported emoji).",
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
}
