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
import { useCallback, useEffect, useRef, useState } from 'react'
import Editor from '@monaco-editor/react'
import { toast } from 'sonner'
import { AlertTriangle, Check, Loader2, Network, RefreshCw, Save, Search, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Switch } from '@/components/ui/switch'
import { Badge } from '@/components/ui/badge'
import {
  getGatewayConfig,
  saveGatewayConfig,
  validateGatewayConfig,
  syncGatewayChannels,
  previewGatewayRoute,
} from './api'
import type { GatewayApplyResult, GatewaySyncResult, GatewayValidateResult, GatewayRoutePreviewResult } from './types'

type ToggleState = {
  routingEnabled: boolean
  affinityEnabled: boolean
  channelAutoSync: boolean
  costSyncEnabled: boolean
}

export function GatewayConfigSettings() {
  const [yaml, setYaml] = useState('')
  const [originalYaml, setOriginalYaml] = useState('')
  const [toggles, setToggles] = useState<ToggleState>({
    routingEnabled: false,
    affinityEnabled: true,
    channelAutoSync: true,
    costSyncEnabled: true,
  })
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [validating, setValidating] = useState(false)
  const [syncing, setSyncing] = useState(false)
  const [validationResult, setValidationResult] = useState<GatewayValidateResult | null>(null)
  const [applyResult, setApplyResult] = useState<GatewayApplyResult | null>(null)
  const [syncResult, setSyncResult] = useState<GatewaySyncResult | null>(null)
  const [previewResult, setPreviewResult] = useState<GatewayRoutePreviewResult | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)
  const editorRef = useRef<unknown>(null)

  const loadConfig = useCallback(async () => {
    setLoading(true)
    try {
      const config = await getGatewayConfig()
      setYaml(config.yaml || '')
      setOriginalYaml(config.yaml || '')
      setToggles({
        routingEnabled: config.enabled === 'true',
        affinityEnabled: true,
        channelAutoSync: true,
        costSyncEnabled: true,
      })
      setApplyResult(null)
      setSyncResult(null)
      setValidationResult(null)
    } catch {
      toast.error('Failed to load gateway config')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    loadConfig()
  }, [loadConfig])

  const handleEditorMount = (editor: unknown) => {
    editorRef.current = editor
  }

  const hasChanges = yaml !== originalYaml

  const handleValidate = async () => {
    setValidating(true)
    setValidationResult(null)
    try {
      const result = await validateGatewayConfig(yaml)
      setValidationResult(result)
      if (result.valid) {
        toast.success('YAML configuration is valid')
      } else {
        toast.error(`Validation failed: ${result.error}`)
      }
    } catch (err) {
      toast.error('Validation request failed')
      setValidationResult({ error: String(err) })
    } finally {
      setValidating(false)
    }
  }

  const handleSave = async () => {
    setSaving(true)
    setApplyResult(null)
    try {
      const result = await saveGatewayConfig(yaml)
      if (result.applied) {
        setApplyResult(result)
        setOriginalYaml(yaml)
        toast.success('Configuration saved and applied successfully', {
          description: `Effective at ${result.effective_at}`,
        })
      } else {
        toast.error('Failed to apply configuration')
      }
    } catch (err) {
      toast.error(`Save failed: ${String(err)}`)
    } finally {
      setSaving(false)
    }
  }

  const handleSync = async () => {
    setSyncing(true)
    setSyncResult(null)
    try {
      const result = await syncGatewayChannels()
      setSyncResult(result)
      if (result.errors.length === 0) {
        toast.success('Channel and cost sync completed')
      } else {
        toast.warning('Sync completed with errors', {
          description: result.errors.join('; '),
        })
      }
    } catch {
      toast.error('Sync request failed')
    } finally {
      setSyncing(false)
    }
  }

  const handlePreviewRoute = async () => {
    setPreviewLoading(true)
    try {
      const result = await previewGatewayRoute({
        group: 'default',
        client_id: '',
        model: 'auto',
      })
      setPreviewResult(result)
    } catch {
      toast.error('Route preview failed')
    } finally {
      setPreviewLoading(false)
    }
  }

  const handleToggleRouting = async (checked: boolean) => {
    setToggles((prev) => ({ ...prev, routingEnabled: checked }))
    try {
      const { api } = await import('@/lib/api')
      await api.put('/api/option/', {
        key: 'GatewayRoutingEnabled',
        value: String(checked),
      })
      toast.success(checked ? 'Intelligent routing enabled' : 'Intelligent routing disabled')
    } catch {
      toast.error('Failed to update routing toggle')
      setToggles((prev) => ({ ...prev, routingEnabled: !checked }))
    }
  }

  if (loading) {
    return (
      <div className="flex items-center justify-center py-20">
        <Loader2 className="size-6 animate-spin text-muted-foreground" />
        <span className="ml-2 text-muted-foreground">Loading gateway configuration...</span>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-2">
        <Network className="size-5 text-primary" />
        <h2 className="text-lg font-semibold">Gateway Routing Configuration</h2>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Feature Toggles</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-2 gap-6">
            <div className="flex items-center justify-between gap-3">
              <div>
                <div className="font-medium">Intelligent Routing</div>
                <div className="text-sm text-muted-foreground">Enable gateway strategy middleware for request routing</div>
              </div>
              <Switch checked={toggles.routingEnabled} onCheckedChange={handleToggleRouting} />
            </div>
            <div className="flex items-center justify-between gap-3">
              <div>
                <div className="font-medium">Session Affinity</div>
                <div className="text-sm text-muted-foreground">Stick same task to same provider/model</div>
              </div>
              <Switch
                checked={toggles.affinityEnabled}
                onCheckedChange={(v) => setToggles((p) => ({ ...p, affinityEnabled: v }))}
              />
            </div>
            <div className="flex items-center justify-between gap-3">
              <div>
                <div className="font-medium">Auto Sync Channels</div>
                <div className="text-sm text-muted-foreground">Automatically sync providers to New API channels</div>
              </div>
              <Switch
                checked={toggles.channelAutoSync}
                onCheckedChange={(v) => setToggles((p) => ({ ...p, channelAutoSync: v }))}
              />
            </div>
            <div className="flex items-center justify-between gap-3">
              <div>
                <div className="font-medium">Sync Cost to Billing</div>
                <div className="text-sm text-muted-foreground">Sync cost config to New API pricing system</div>
              </div>
              <Switch
                checked={toggles.costSyncEnabled}
                onCheckedChange={(v) => setToggles((p) => ({ ...p, costSyncEnabled: v }))}
              />
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex-row items-center justify-between">
          <CardTitle>YAML Configuration</CardTitle>
          {hasChanges && (
            <Badge variant="outline" className="text-warning border-warning/30">
              Unsaved changes
            </Badge>
          )}
        </CardHeader>
        <CardContent>
          {validationResult && !validationResult.valid && validationResult.error && (
            <div className="mb-3 flex items-center gap-2 rounded-lg bg-destructive/10 p-3 text-sm text-destructive">
              <AlertTriangle className="size-4" />
              <span>
                {validationResult.line && validationResult.line > 0
                  ? `Line ${validationResult.line}: `
                  : ''}
                {validationResult.error}
              </span>
              <Button variant="ghost" size="sm" className="ml-auto h-6 px-2" onClick={() => setValidationResult(null)}>
                <X className="size-3" />
              </Button>
            </div>
          )}
          {validationResult?.valid && (
            <div className="mb-3 flex items-center gap-2 rounded-lg bg-emerald-500/10 p-3 text-sm text-emerald-600">
              <Check className="size-4" />
              <span>YAML is valid</span>
              <Button variant="ghost" size="sm" className="ml-auto h-6 px-2" onClick={() => setValidationResult(null)}>
                <X className="size-3" />
              </Button>
            </div>
          )}
          <div className="rounded-lg border bg-background">
            <Editor
              height="500px"
              language="yaml"
              value={yaml}
              onChange={(value) => setYaml(value || '')}
              onMount={handleEditorMount}
              theme="vs-dark"
              options={{
                minimap: { enabled: false },
                fontSize: 13,
                lineNumbers: 'on',
                scrollBeyondLastLine: false,
                automaticLayout: true,
                tabSize: 2,
                wordWrap: 'on',
                folding: true,
                renderLineHighlight: 'all',
                suggestOnTriggerCharacters: false,
              }}
            />
          </div>
        </CardContent>
      </Card>

      <div className="flex items-center gap-3">
        <Button variant="outline" onClick={handleValidate} disabled={validating || !yaml}>
          {validating ? <Loader2 className="size-4 animate-spin" /> : <Search className="size-4" />}
          Validate
        </Button>
        <Button variant="outline" onClick={handlePreviewRoute} disabled={previewLoading}>
          {previewLoading ? <Loader2 className="size-4 animate-spin" /> : <Search className="size-4" />}
          Preview Route
        </Button>
        <Button variant="outline" onClick={handleSync} disabled={syncing}>
          {syncing ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
          Sync Channels
        </Button>
        <Button onClick={handleSave} disabled={saving || !hasChanges}>
          {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
          Save & Apply
        </Button>
        <Button variant="ghost" onClick={loadConfig}>
          <RefreshCw className="size-4" />
          Reload
        </Button>
      </div>

      {(applyResult || syncResult) && (
        <Card>
          <CardHeader>
            <CardTitle>Sync Results</CardTitle>
          </CardHeader>
          <CardContent>
            {applyResult && (
              <div className="space-y-2 text-sm">
                <div className="flex items-center gap-2">
                  <Badge variant="outline">{applyResult.applied ? 'Applied' : 'Failed'}</Badge>
                  <span className="text-muted-foreground">Effective at: {applyResult.effective_at}</span>
                </div>
                {applyResult.channels_created && applyResult.channels_created.length > 0 && (
                  <div className="text-emerald-600">+ Created channels: {applyResult.channels_created.join(', ')}</div>
                )}
                {applyResult.channels_updated && applyResult.channels_updated.length > 0 && (
                  <div className="text-blue-600">~ Updated channels: {applyResult.channels_updated.join(', ')}</div>
                )}
                {applyResult.channels_disabled && applyResult.channels_disabled.length > 0 && (
                  <div className="text-destructive">- Disabled channels: {applyResult.channels_disabled.join(', ')}</div>
                )}
                {applyResult.pricing_updated > 0 && (
                  <div className="text-blue-600">~ Updated pricing: {applyResult.pricing_updated} models</div>
                )}
                {applyResult.errors && applyResult.errors.length > 0 && (
                  <div className="text-destructive">Errors: {applyResult.errors.join('; ')}</div>
                )}
              </div>
            )}
            {syncResult && (
              <div className="space-y-2 text-sm">
                {syncResult.channels.Created && syncResult.channels.Created.length > 0 && (
                  <div className="text-emerald-600">+ Created: {syncResult.channels.Created.join(', ')}</div>
                )}
                {syncResult.channels.Updated && syncResult.channels.Updated.length > 0 && (
                  <div className="text-blue-600">~ Updated: {syncResult.channels.Updated.join(', ')}</div>
                )}
                {syncResult.channels.Disabled && syncResult.channels.Disabled.length > 0 && (
                  <div className="text-destructive">- Disabled: {syncResult.channels.Disabled.join(', ')}</div>
                )}
                <div className="text-blue-600">~ Pricing updated: {syncResult.cost.Updated} models</div>
                {syncResult.errors.length > 0 && (
                  <div className="text-destructive">Errors: {syncResult.errors.join('; ')}</div>
                )}
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {previewResult && (
        <Card>
          <CardHeader className="flex-row items-center justify-between">
            <CardTitle>Route Preview</CardTitle>
            <Button variant="ghost" size="sm" onClick={() => setPreviewResult(null)}>
              <X className="size-4" />
            </Button>
          </CardHeader>
          <CardContent>
            <div className="space-y-3 text-sm">
              <div className="flex gap-4 text-muted-foreground">
                <span>Group: <strong>{previewResult.group}</strong></span>
                <span>Strategy: <strong>{previewResult.strategy}</strong></span>
                <span>Affinity hit: <strong>{String(previewResult.affinity_hit)}</strong></span>
              </div>
              <div className="rounded-lg border p-3">
                <div className="mb-2 font-medium">Candidate Chain ({previewResult.candidates.length} candidates)</div>
                <div className="space-y-1">
                  {previewResult.candidates.map((c, i) => (
                    <div key={i} className="flex items-center gap-2 py-0.5">
                      <span className="inline-block w-6 text-right text-muted-foreground">{i + 1}.</span>
                      <Badge variant="outline" className="text-xs">{c.ProviderId}</Badge>
                      <span>{c.ActualModel}</span>
                      {c.IsFree && <Badge className="text-xs bg-emerald-500/10 text-emerald-600 border-emerald-500/30">free</Badge>}
                    </div>
                  ))}
                </div>
              </div>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
