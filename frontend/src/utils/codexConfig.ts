export const SUB2API_CODEX_PROVIDER_ID = 'custom'
export const SUB2API_CODEX_PROVIDER_NAME = 'Sub2API'
export const SUB2API_CODEX_MODEL = 'gpt-5.6-sol'
export const SUB2API_CODEX_REVIEW_MODEL = 'gpt-5.5'

export type CodexAuthMode = 'legacy' | 'api-key'

interface BuildCodexConfigTomlOptions {
  baseUrl: string
  authMode: CodexAuthMode
  websocket?: boolean
}

export function buildCodexConfigToml({
  baseUrl,
  authMode,
  websocket = false
}: BuildCodexConfigTomlOptions): string {
  const authConfig = authMode === 'api-key'
    ? `requires_openai_auth = false
http_headers = { "x-openai-actor-authorization" = "local-image-extension" }`
    : 'requires_openai_auth = true'
  const websocketConfig = websocket ? '\nsupports_websockets = true' : ''
  const featuresConfig = websocket
    ? 'responses_websockets_v2 = true\ngoals = true'
    : 'goals = true'

  return `model_provider = "${SUB2API_CODEX_PROVIDER_ID}"
model = "${SUB2API_CODEX_MODEL}"
review_model = "${SUB2API_CODEX_REVIEW_MODEL}"
model_reasoning_effort = "xhigh"
disable_response_storage = true
network_access = "enabled"
windows_wsl_setup_acknowledged = true

[model_providers.${SUB2API_CODEX_PROVIDER_ID}]
name = "${SUB2API_CODEX_PROVIDER_NAME}"
base_url = "${baseUrl}"
wire_api = "responses"
${authConfig}${websocketConfig}

[features]
${featuresConfig}`
}
