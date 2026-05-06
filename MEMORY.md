# MEMORY.md - Long-Term Memory

## Marc
- **Model preference:** Use openai-codex for chat by default. Use sub-agents for heavier coding work when useful.
- Based in Austin, TX (Central Time)
- LA area code (+1 310-871-6614) but lives in Austin
- Telegram ID: 7583257477
- Wants me direct and productive — push back when wrong
- Informal is fine, fluff is not
- Running on Max plan (Opus 4.5)
- Wife: Lisa Campbell

## Me (Rooty)
- Born 2026-01-25
- 🌱 grounded, resourceful, grows into everything
- Primary channel: Telegram (@MarcCClawdBot)
- Running on dedicated Hetzner VPS (migrated Jan 2026)
- **Solo operation now** — no bot fleet to manage (all retired 2026-04-02)

## Working Integrations
- **ElevenLabs Voice Calls**: Can make outbound calls to businesses (reservations, inquiries, etc.)
  - Agent ID: agent_5701kftq9fkdftfttb3vtpztc4ms
  - Outbound number: +13103629099
- **Headless Browser**: Playwright/Chromium on VPS for web automation
- **Telegram**: Bot configured and working

## Lessons Learned
- ElevenLabs first_message override requires nested structure in `conversation_initiation_client_data`
- Google OAuth blocks headless browsers for security - use phone/email login instead
- Sprouts phone number (512) 912-1470 didn't connect - may need verification
- Most Sprouts stores: 7am-10pm daily
- **NEVER force push** — Marc explicitly wants to see every commit. I keep doing this despite the rule. Read `NEVER_FORCE_PUSH.md` before any git push on a branch that exists on origin. If a branch needs fixes after pushing: new commit, normal push. No amend, no rebase, no force.

## Infrastructure: plausible-01
- VPS: CPX41 (16GB RAM, 8 vCPUs) in Hillsboro, OR
- Tailscale IP: **100.85.148.103**
- Admin access: `http://plausible-01/`
- Public tracking: `https://plausible.machination.dev`
- Upgraded from CPX21 on 2026-02-25
