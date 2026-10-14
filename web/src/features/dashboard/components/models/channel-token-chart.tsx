/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { getChannelQuotaDates } from '@/features/dashboard/api'
import { buildQueryParams, getDefaultDays } from '@/features/dashboard/lib'
import type {
  ChannelQuotaDataItem,
  DashboardFilters,
  QuotaDataItem,
} from '@/features/dashboard/types'
import { computeTimeRange } from '@/lib/time'

import { ModelTokenChart } from './model-token-chart'

interface ChannelTokenChartProps {
  filters?: DashboardFilters
  timeGranularity?: DashboardFilters['time_granularity']
}

// Map channel-aggregated rows onto the model chart shape so the ranking
// bar/tooltip/stacking logic is reused verbatim; channel_id=0 (legacy rows
// before channel tracking) shows up as "Unknown".
function toQuotaDataItems(
  rows: ChannelQuotaDataItem[],
  unknownLabel: string
): QuotaDataItem[] {
  return rows.map((row) => ({
    model_name: row.channel_name || unknownLabel,
    created_at: row.created_at,
    token_used: row.token_used,
    count: row.count,
    quota: row.quota,
    prompt_tokens: row.prompt_tokens,
    completion_tokens: row.completion_tokens,
    cache_tokens: row.cache_tokens,
  }))
}

export function ChannelTokenChart(props: ChannelTokenChartProps) {
  const { t } = useTranslation()
  const [data, setData] = useState<ChannelQuotaDataItem[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    const abortController = new AbortController()
    setLoading(true)

    const timeRange = computeTimeRange(
      getDefaultDays(props.filters?.time_granularity),
      props.filters?.start_timestamp,
      props.filters?.end_timestamp
    )

    void getChannelQuotaDates(buildQueryParams(timeRange, props.filters))
      .then((res) => {
        if (abortController.signal.aborted) return
        setData(res?.data || [])
      })
      .catch(() => {
        if (abortController.signal.aborted) return
        setData([])
      })
      .finally(() => {
        if (!abortController.signal.aborted) setLoading(false)
      })

    return () => {
      abortController.abort()
    }
  }, [props.filters])

  return (
    <ModelTokenChart
      data={toQuotaDataItems(data, t('Unknown'))}
      loading={loading}
      timeGranularity={props.timeGranularity}
      titleKey='Channel Token Usage Ranking'
    />
  )
}
