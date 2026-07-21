/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or ( at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
export type GatewayConfigResponse = {
  yaml: string
  enabled: string
  config_path: string
}

export type GatewayApplyResult = {
  applied: boolean
  channels_created: string[] | null
  channels_updated: string[] | null
  channels_disabled: string[] | null
  pricing_updated: number
  effective_at: string
  errors?: string[]
}

export type GatewayValidateResult = {
  valid?: boolean
  error?: string
  line?: number
}

export type GatewaySyncResult = {
  channels: {
    Created: string[] | null
    Updated: string[] | null
    Disabled: string[] | null
  }
  cost: {
    Updated: number
  }
  errors: string[]
}

export type GatewayRoutePreviewRequest = {
  group: string
  client_id: string
  model: string
}

export type GatewayCandidate = {
  ProviderId: string
  ProviderName: string
  ChannelId: number
  KeyIndex: number
  Keys: string[]
  ModelGroup: string
  ActualModel: string
  IsFree: boolean
}

export type GatewayRoutePreviewResult = {
  group: string
  client_id: string
  model: string
  strategy: string
  candidates: GatewayCandidate[]
  affinity_hit: boolean
}

export type GatewayAffinityResponse = {
  bindings: unknown[]
}

export type GatewayStatsResponse = {
  total_requests: number
  affinity_hits: number
  affinity_misses: number
  model_switches: number
  provider_requests: Record<string, number>
  model_requests: Record<string, number>
  retry_counts: Record<string, number>
}
