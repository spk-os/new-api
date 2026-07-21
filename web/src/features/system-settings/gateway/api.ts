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
import { api } from '@/lib/api'
import type {
  GatewayConfigResponse,
  GatewayApplyResult,
  GatewayValidateResult,
  GatewaySyncResult,
  GatewayRoutePreviewRequest,
  GatewayRoutePreviewResult,
  GatewayStatsResponse,
} from './types'

export async function getGatewayConfig() {
  const res = await api.get<GatewayConfigResponse>(
    '/api/admin/gateway/config',
  )
  return res.data
}

export async function saveGatewayConfig(yaml: string) {
  const res = await api.put<GatewayApplyResult>(
    '/api/admin/gateway/config',
    { yaml },
  )
  return res.data
}

export async function validateGatewayConfig(yaml: string) {
  const res = await api.post<GatewayValidateResult>(
    '/api/admin/gateway/config/validate',
    { yaml },
  )
  return res.data
}

export async function syncGatewayChannels() {
  const res = await api.post<GatewaySyncResult>(
    '/api/admin/gateway/config/sync',
  )
  return res.data
}

export async function previewGatewayRoute(
  request: GatewayRoutePreviewRequest,
) {
  const res = await api.post<GatewayRoutePreviewResult>(
    '/api/admin/gateway/route/preview',
    request,
  )
  return res.data
}

export async function getGatewayStats() {
  const res = await api.get<GatewayStatsResponse>(
    '/api/admin/gateway/stats',
  )
  return res.data
}
