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
import Editor from '@monaco-editor/react'
import {
  AlertTriangle,
  Check,
  Loader2,
  Maximize2,
  Minimize2,
  Network,
  RefreshCw,
  Save,
  Search,
  X,
} from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Switch } from '@/components/ui/switch'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

import {
  getGatewayConfig,
  saveGatewayConfig,
  validateGatewayConfig,
  syncGatewayChannels,
  previewGatewayRoute,
} from './api'
import type {
  GatewayApplyResult,
  GatewaySyncResult,
  GatewayValidateResult,
  GatewayRoutePreviewResult,
} from './types'
import {
  applyTabEdit,
  joinYaml,
  splitYaml,
  tabDefs,
  tabText,
  type Section,
} from './yaml-sections'

type ToggleState = {
  routingEnabled: boolean
  affinityEnabled: boolean
  channelAutoSync: boolean
  costSyncEnabled: boolean
}

const EDITOR_OPTIONS = {
  minimap: { enabled: false },
  fontSize: 13,
  lineNumbers: 'on' as const,
  scrollBeyondLastLine: false,
  automaticLayout: true,
  tabSize: 2,
  wordWrap: 'on' as const,
  folding: true,
  renderLineHighlight: 'all' as const,
  suggestOnTriggerCharacters: false,
}
export function GatewayConfigSettings() {
  const { t } = useTranslation()
  const [sections, setSections] = useState<Section[]>([])
  const [originalYaml, setOriginalYaml] = useState('')
  const [activeTab, setActiveTab] = useState('llm_gateway')
  const [fullscreen, setFullscreen] = useState(false)
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
  const [validationResult, setValidationResult] =
    useState<GatewayValidateResult | null>(null)
  const [applyResult, setApplyResult] = useState<GatewayApplyResult | null>(
    null
  )
  const [syncResult, setSyncResult] = useState<GatewaySyncResult | null>(null)
  const [previewResult, setPreviewResult] =
    useState<GatewayRoutePreviewResult | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)
  const editorRef = useRef<unknown>(null)

  const loadConfig = useCallback(async () => {
    setLoading(true)
    try {
      const config = await getGatewayConfig()
      const text = config.yaml || ''
      setSections(splitYaml(text))
      setOriginalYaml(text)
      setActiveTab('llm_gateway')
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
      toast.error(t('Failed to load gateway config'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => {
    loadConfig()
  }, [loadConfig])

  // ESC exits the fullscreen overlay.
  useEffect(() => {
    if (!fullscreen) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setFullscreen(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [fullscreen])

  const handleEditorMount = (editor: unknown) => {
    editorRef.current = editor
  }

  const yaml = joinYaml(sections)
  const hasChanges = yaml !== originalYaml
  const tabs = tabDefs(sections)
  const editorValue = tabText(sections, activeTab)
  const handleEditorChange = (value: string | undefined) => {
    setSections((prev) => applyTabEdit(prev, activeTab, value || ''))
  }
  const handleValidate = async () => {
    setValidating(true)
    setValidationResult(null)
    try {
      const result = await validateGatewayConfig(yaml)
      setValidationResult(result)
      if (result.valid) {
        toast.success(t('YAML configuration is valid'))
      } else {
        toast.error(t('Validation failed: {{error}}', { error: result.error }))
      }
    } catch (err) {
      toast.error(t('Validation request failed'))
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
        toast.success(t('Configuration saved and applied successfully'), {
          description: t('Effective at {{time}}', {
            time: result.effective_at,
          }),
        })
      } else {
        toast.error(t('Failed to apply configuration'))
      }
    } catch (err) {
      toast.error(t('Save failed: {{error}}', { error: String(err) }))
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
        toast.success(t('Channel and cost sync completed'))
      } else {
        toast.warning(t('Sync completed with errors'), {
          description: result.errors.join('; '),
        })
      }
    } catch {
      toast.error(t('Sync request failed'))
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
      toast.error(t('Route preview failed'))
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
      toast.success(
        checked
          ? t('Intelligent routing enabled')
          : t('Intelligent routing disabled')
      )
    } catch {
      toast.error(t('Failed to update routing toggle'))
      setToggles((prev) => ({ ...prev, routingEnabled: !checked }))
    }
  }
  if (loading) {
    return (
      <div className='flex items-center justify-center py-20'>
        <Loader2 className='text-muted-foreground size-6 animate-spin' />
        <span className='text-muted-foreground ml-2'>
          {t('Loading gateway configuration...')}
        </span>
      </div>
    )
  }

  const validationBanner =
    validationResult && !validationResult.valid && validationResult.error ? (
      <div className='bg-destructive/10 text-destructive mb-3 flex items-center gap-2 rounded-lg p-3 text-sm'>
        <AlertTriangle className='size-4' />
        <span>
          {validationResult.line && validationResult.line > 0
            ? t('Line {{n}}:', { n: validationResult.line }) + ' '
            : ''}
          {validationResult.error}
        </span>
        <Button
          variant='ghost'
          size='sm'
          className='ml-auto h-6 px-2'
          onClick={() => setValidationResult(null)}
        >
          <X className='size-3' />
        </Button>
      </div>
    ) : validationResult?.valid ? (
      <div className='mb-3 flex items-center gap-2 rounded-lg bg-emerald-500/10 p-3 text-sm text-emerald-600'>
        <Check className='size-4' />
        <span>{t('YAML is valid')}</span>
        <Button
          variant='ghost'
          size='sm'
          className='ml-auto h-6 px-2'
          onClick={() => setValidationResult(null)}
        >
          <X className='size-3' />
        </Button>
      </div>
    ) : null

  const actionsRow = (
    <div className='flex flex-wrap items-center gap-3'>
      <Button
        variant='outline'
        onClick={handleValidate}
        disabled={validating || !yaml}
      >
        {validating ? (
          <Loader2 className='size-4 animate-spin' />
        ) : (
          <Search className='size-4' />
        )}
        {t('Validate')}
      </Button>
      <Button
        variant='outline'
        onClick={handlePreviewRoute}
        disabled={previewLoading}
      >
        {previewLoading ? (
          <Loader2 className='size-4 animate-spin' />
        ) : (
          <Search className='size-4' />
        )}
        {t('Preview Route')}
      </Button>
      <Button variant='outline' onClick={handleSync} disabled={syncing}>
        {syncing ? (
          <Loader2 className='size-4 animate-spin' />
        ) : (
          <RefreshCw className='size-4' />
        )}
        {t('Sync Channels')}
      </Button>
      <Button onClick={handleSave} disabled={saving || !hasChanges}>
        {saving ? (
          <Loader2 className='size-4 animate-spin' />
        ) : (
          <Save className='size-4' />
        )}
        {t('Save & Apply')}
      </Button>
      <Button variant='ghost' onClick={loadConfig}>
        <RefreshCw className='size-4' />
        {t('Reload')}
      </Button>
    </div>
  )

  const editorBlock = (
    <div
      className={
        fullscreen
          ? 'bg-background flex min-h-0 flex-1 flex-col rounded-lg border'
          : 'bg-background h-[1000px] rounded-lg border'
      }
    >
      <Editor
        height='100%'
        language='yaml'
        value={editorValue}
        onChange={handleEditorChange}
        onMount={handleEditorMount}
        theme='vs-dark'
        options={EDITOR_OPTIONS}
      />
    </div>
  )

  const tabsBar = (
    <Tabs
      value={activeTab}
      onValueChange={setActiveTab}
      className={
        fullscreen
          ? 'flex min-h-0 flex-1 flex-col gap-2'
          : 'flex flex-col gap-2'
      }
    >
      <TabsList className='w-full overflow-x-auto'>
        {tabs.map((tab) => (
          <TabsTrigger key={tab.value} value={tab.value}>
            {t(tab.label)}
          </TabsTrigger>
        ))}
      </TabsList>
      <TabsContent
        value={activeTab}
        className={fullscreen ? 'flex min-h-0 flex-1 flex-col' : ''}
      >
        {validationBanner}
        {editorBlock}
      </TabsContent>
    </Tabs>
  )
  return (
    <div className='min-h-0 flex-1 space-y-6 overflow-auto'>
      <div className='flex items-center gap-2'>
        <Network className='text-primary size-5' />
        <h2 className='text-lg font-semibold'>
          {t('Gateway Routing Configuration')}
        </h2>
      </div>

      <Card>
        <CardHeader className='flex-row items-center justify-between'>
          <div className='flex items-center gap-2'>
            <CardTitle>{t('YAML Configuration')}</CardTitle>
            {hasChanges && (
              <Badge
                variant='outline'
                className='text-warning border-warning/30'
              >
                {t('Unsaved changes')}
              </Badge>
            )}
          </div>
          <Button
            variant='ghost'
            size='sm'
            onClick={() => setFullscreen(true)}
            title={t('Enter fullscreen')}
          >
            <Maximize2 className='size-4' />
            {t('Enter fullscreen')}
          </Button>
        </CardHeader>
        <CardContent>{tabsBar}</CardContent>
      </Card>

      {actionsRow}

      {(applyResult || syncResult) && (
        <Card>
          <CardHeader>
            <CardTitle>{t('Sync Results')}</CardTitle>
          </CardHeader>
          <CardContent>
            {applyResult && (
              <div className='space-y-2 text-sm'>
                <div className='flex items-center gap-2'>
                  <Badge variant='outline'>
                    {applyResult.applied ? t('Applied') : t('Failed')}
                  </Badge>
                  <span className='text-muted-foreground'>
                    {t('Effective at:')} {applyResult.effective_at}
                  </span>
                </div>
                {applyResult.channels_created &&
                  applyResult.channels_created.length > 0 && (
                    <div className='text-emerald-600'>
                      + {t('Created channels')}:{' '}
                      {applyResult.channels_created.join(', ')}
                    </div>
                  )}
                {applyResult.channels_updated &&
                  applyResult.channels_updated.length > 0 && (
                    <div className='text-blue-600'>
                      ~ {t('Updated channels')}:{' '}
                      {applyResult.channels_updated.join(', ')}
                    </div>
                  )}
                {applyResult.channels_disabled &&
                  applyResult.channels_disabled.length > 0 && (
                    <div className='text-destructive'>
                      - {t('Disabled channels')}:{' '}
                      {applyResult.channels_disabled.join(', ')}
                    </div>
                  )}
                {applyResult.pricing_updated > 0 && (
                  <div className='text-blue-600'>
                    ~ {t('Updated pricing')}: {applyResult.pricing_updated}{' '}
                    {t('models')}
                  </div>
                )}
                {applyResult.errors && applyResult.errors.length > 0 && (
                  <div className='text-destructive'>
                    {t('Errors')}: {applyResult.errors.join('; ')}
                  </div>
                )}
              </div>
            )}
            {syncResult && (
              <div className='space-y-2 text-sm'>
                {syncResult.channels.Created &&
                  syncResult.channels.Created.length > 0 && (
                    <div className='text-emerald-600'>
                      + {t('Created')}: {syncResult.channels.Created.join(', ')}
                    </div>
                  )}
                {syncResult.channels.Updated &&
                  syncResult.channels.Updated.length > 0 && (
                    <div className='text-blue-600'>
                      ~ {t('Updated')}: {syncResult.channels.Updated.join(', ')}
                    </div>
                  )}
                {syncResult.channels.Disabled &&
                  syncResult.channels.Disabled.length > 0 && (
                    <div className='text-destructive'>
                      - {t('Disabled')}:{' '}
                      {syncResult.channels.Disabled.join(', ')}
                    </div>
                  )}
                <div className='text-blue-600'>
                  ~ {t('Pricing updated')}: {syncResult.cost.Updated}{' '}
                  {t('models')}
                </div>
                {syncResult.errors.length > 0 && (
                  <div className='text-destructive'>
                    {t('Errors')}: {syncResult.errors.join('; ')}
                  </div>
                )}
              </div>
            )}
          </CardContent>
        </Card>
      )}
      {previewResult && (
        <Card>
          <CardHeader className='flex-row items-center justify-between'>
            <CardTitle>{t('Route Preview')}</CardTitle>
            <Button
              variant='ghost'
              size='sm'
              onClick={() => setPreviewResult(null)}
            >
              <X className='size-4' />
            </Button>
          </CardHeader>
          <CardContent>
            <div className='space-y-3 text-sm'>
              <div className='text-muted-foreground flex gap-4'>
                <span>
                  {t('Group')}: <strong>{previewResult.group}</strong>
                </span>
                <span>
                  {t('Strategy')}: <strong>{previewResult.strategy}</strong>
                </span>
                <span>
                  {t('Affinity hit')}:{' '}
                  <strong>{String(previewResult.affinity_hit)}</strong>
                </span>
              </div>
              <div className='rounded-lg border p-3'>
                <div className='mb-2 font-medium'>
                  {t('Candidate Chain')} ({previewResult.candidates.length}{' '}
                  {t('candidates')})
                </div>
                <div className='space-y-1'>
                  {previewResult.candidates.map((c, i) => (
                    <div key={i} className='flex items-center gap-2 py-0.5'>
                      <span className='text-muted-foreground inline-block w-6 text-right'>
                        {i + 1}.
                      </span>
                      <Badge variant='outline' className='text-xs'>
                        {c.ProviderId}
                      </Badge>
                      <span>{c.ActualModel}</span>
                      {c.IsFree && (
                        <Badge className='border-emerald-500/30 bg-emerald-500/10 text-xs text-emerald-600'>
                          {t('free')}
                        </Badge>
                      )}
                    </div>
                  ))}
                </div>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>{t('Feature Toggles')}</CardTitle>
        </CardHeader>
        <CardContent>
          <div className='grid grid-cols-2 gap-4 sm:grid-cols-4'>
            <div
              className='flex flex-col gap-1'
              title={t(
                'Enable gateway strategy middleware for request routing'
              )}
            >
              <div className='flex items-center justify-between gap-2'>
                <span className='font-medium'>{t('Intelligent Routing')}</span>
                <Switch
                  checked={toggles.routingEnabled}
                  onCheckedChange={handleToggleRouting}
                />
              </div>
              <span className='text-muted-foreground text-xs'>
                {t('Enable gateway strategy middleware for request routing')}
              </span>
            </div>
            <div
              className='flex flex-col gap-1'
              title={t('Stick same task to same provider/model')}
            >
              <div className='flex items-center justify-between gap-2'>
                <span className='font-medium'>{t('Session Affinity')}</span>
                <Switch
                  checked={toggles.affinityEnabled}
                  onCheckedChange={(v) =>
                    setToggles((p) => ({ ...p, affinityEnabled: v }))
                  }
                />
              </div>
              <span className='text-muted-foreground text-xs'>
                {t('Stick same task to same provider/model')}
              </span>
            </div>
            <div
              className='flex flex-col gap-1'
              title={t('Automatically sync providers to New API channels')}
            >
              <div className='flex items-center justify-between gap-2'>
                <span className='font-medium'>{t('Auto Sync Channels')}</span>
                <Switch
                  checked={toggles.channelAutoSync}
                  onCheckedChange={(v) =>
                    setToggles((p) => ({ ...p, channelAutoSync: v }))
                  }
                />
              </div>
              <span className='text-muted-foreground text-xs'>
                {t('Automatically sync providers to New API channels')}
              </span>
            </div>
            <div
              className='flex flex-col gap-1'
              title={t('Sync cost config to New API pricing system')}
            >
              <div className='flex items-center justify-between gap-2'>
                <span className='font-medium'>{t('Sync Cost to Billing')}</span>
                <Switch
                  checked={toggles.costSyncEnabled}
                  onCheckedChange={(v) =>
                    setToggles((p) => ({ ...p, costSyncEnabled: v }))
                  }
                />
              </div>
              <span className='text-muted-foreground text-xs'>
                {t('Sync cost config to New API pricing system')}
              </span>
            </div>
          </div>
        </CardContent>
      </Card>

      {fullscreen && (
        <div className='bg-background fixed inset-0 z-50 flex flex-col p-4'>
          <div className='mb-3 flex items-center justify-between'>
            <h2 className='text-lg font-semibold'>{t('YAML Configuration')}</h2>
            <Button
              variant='ghost'
              size='sm'
              onClick={() => setFullscreen(false)}
              title={t('Exit fullscreen')}
            >
              <Minimize2 className='size-4' />
              {t('Exit fullscreen')}
            </Button>
          </div>
          {tabsBar}
          <div className='mt-3'>{actionsRow}</div>
        </div>
      )}
    </div>
  )
}
