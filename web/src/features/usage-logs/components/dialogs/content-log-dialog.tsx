import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Copy, Check, Loader2, AlertCircle } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { getContentLog } from '../../api'

interface HttpMessage {
  method?: string
  url?: string
  path?: string
  status?: number
  headers?: Record<string, string>
  body?: string
}

interface ContentLogEntry {
  request_id: string
  user_id: number
  channel_id: number
  channel_name: string
  model_name: string
  upstream_model_name?: string
  created_at: number
  gateway_request?: HttpMessage
  gateway_response?: HttpMessage
  upstream_request?: HttpMessage
  upstream_response?: HttpMessage
}

interface ContentLogDialogProps {
  requestId: string
  open: boolean
  onOpenChange: (open: boolean) => void
}

function DetailRow(props: { label: string; value: React.ReactNode; mono?: boolean }) {
  return (
    <div className='grid min-w-0 grid-cols-[5.25rem_minmax(0,1fr)] gap-2 text-sm sm:grid-cols-[7rem_minmax(0,1fr)] sm:gap-3'>
      <span className='text-muted-foreground min-w-0 text-xs'>{props.label}</span>
      <span className={`max-w-full min-w-0 text-xs break-all sm:break-words ${props.mono ? 'font-mono' : ''}`}>
        {props.value}
      </span>
    </div>
  )
}

function Section(props: { label: string; icon?: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className='min-w-0 space-y-1.5'>
      <Label className='flex items-center gap-1.5 text-xs font-semibold'>
        {props.icon}
        {props.label}
      </Label>
      <div className='bg-muted/30 min-w-0 space-y-1 overflow-hidden rounded-md border p-2.5 max-sm:p-2'>
        {props.children}
      </div>
    </div>
  )
}

function CopyButton(props: { text: string }) {
  const { t } = useTranslation()
  const { copiedText, copyToClipboard } = useCopyToClipboard({ notify: false })
  return (
    <Button
      variant='ghost'
      size='sm'
      className='absolute top-1.5 right-1.5 h-5 w-5 p-0'
      onClick={() => copyToClipboard(props.text)}
      title={t('Copy to clipboard')}
      aria-label={t('Copy to clipboard')}
    >
      {copiedText === props.text ? (
        <Check className='size-3 text-green-600' />
      ) : (
        <Copy className='size-3' />
      )}
    </Button>
  )
}

function MessageView(props: { msg: HttpMessage; title: string }) {
  const { msg, title } = props
  const { t } = useTranslation()
  const formattedBody = msg.body
    ? (() => {
        try {
          return JSON.stringify(JSON.parse(msg.body), null, 2)
        } catch {
          return msg.body
        }
      })()
    : ''

  return (
    <Section label={title}>
      {msg.method && <DetailRow label={t('Method')} value={msg.method} mono />}
      {msg.url && <DetailRow label='URL' value={msg.url} mono />}
      {msg.path && <DetailRow label={t('Path')} value={msg.path} mono />}
      {msg.status != null && (
        <DetailRow label={t('Status')} value={String(msg.status)} mono />
      )}
      {msg.headers && Object.keys(msg.headers).length > 0 && (
        <div className='min-w-0 space-y-0.5'>
          <span className='text-muted-foreground text-xs'>{t('Headers')}</span>
          <div className='relative min-w-0'>
            <CopyButton text={JSON.stringify(msg.headers, null, 2)} />
            <pre className='bg-background/60 max-h-32 overflow-y-auto rounded border p-2 font-mono text-[11px] leading-relaxed break-all whitespace-pre-wrap'>
              {Object.entries(msg.headers).map(([k, v]) => `${k}: ${v}`).join('\n')}
            </pre>
          </div>
        </div>
      )}
      {msg.body && (
        <div className='min-w-0 space-y-0.5'>
          <span className='text-muted-foreground text-xs'>{t('Body')}</span>
          <div className='relative min-w-0'>
            <CopyButton text={msg.body} />
            <pre className='bg-background/60 max-h-60 overflow-y-auto rounded border p-2 font-mono text-[11px] leading-relaxed break-all whitespace-pre-wrap'>
              {formattedBody}
            </pre>
          </div>
        </div>
      )}
    </Section>
  )
}

export function ContentLogDialog(props: ContentLogDialogProps) {
  const { t } = useTranslation()
  const [entry, setEntry] = useState<ContentLogEntry | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!props.open || !props.requestId) return
    setLoading(true)
    setError('')
    setEntry(null)
    getContentLog(props.requestId)
      .then((res: { success: boolean; data?: ContentLogEntry; message?: string }) => {
        if (res.success && res.data) {
          setEntry(res.data)
        } else {
          setError(res.message || t('Content log not found'))
        }
      })
      .catch((err: Error) => {
        setError(err.message || t('Failed to load content log'))
      })
      .finally(() => setLoading(false))
  }, [props.open, props.requestId, t])

  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent
        className='flex flex-col rounded-xl p-0'
        style={{
          width: 'calc(100vw - 1rem)',
          maxWidth: '1440px',
          height: 'calc(100vh - 1rem)',
          maxHeight: '90vh',
        }}
      >
        <DialogHeader className='shrink-0 px-4 pt-4 pb-0 max-sm:gap-1'>
          <DialogTitle className='flex items-center gap-2 text-base'>
            {t('Content Log')}
          </DialogTitle>
        </DialogHeader>
        <div className='min-h-0 flex-1 overflow-y-auto px-4 pb-4'>
          {loading && (
            <div className='flex items-center justify-center py-8'>
              <Loader2 className='size-6 animate-spin text-muted-foreground' />
            </div>
          )}
          {error && (
            <div className='flex items-center gap-2 py-4 text-sm text-red-500'>
              <AlertCircle className='size-4' />
              {error}
            </div>
          )}
          {entry && (
            <div className='w-full max-w-full min-w-0 space-y-2.5 py-1 sm:space-y-3'>
              {/* Overview */}
              <div className='min-w-0 space-y-1'>
                <DetailRow label={t('Request ID')} value={entry.request_id} mono />
                <DetailRow label={t('Channel')} value={`${entry.channel_name || ''} #${entry.channel_id}`} mono />
                <DetailRow label={t('Model')} value={entry.model_name} />
                {entry.upstream_model_name && (
                  <DetailRow label={t('Upstream Model')} value={entry.upstream_model_name} />
                )}
              </div>

              {/* Gateway Request */}
              {entry.gateway_request && (
                <MessageView msg={entry.gateway_request} title={t('Gateway Request')} />
              )}

              {/* Gateway Response */}
              {entry.gateway_response && (
                <MessageView msg={entry.gateway_response} title={t('Gateway Response')} />
              )}

              {/* Upstream Request */}
              {entry.upstream_request && (
                <MessageView msg={entry.upstream_request} title={t('Upstream Request')} />
              )}

              {/* Upstream Response */}
              {entry.upstream_response ? (
                <MessageView msg={entry.upstream_response} title={t('Upstream Response')} />
              ) : entry.gateway_response && entry.gateway_response.body ? (
                <div className='min-w-0 space-y-1.5'>
                  <div className='bg-muted/30 min-w-0 overflow-hidden rounded-md border p-2.5 max-sm:p-2'>
                    <span className='text-muted-foreground text-xs italic'>
                      {t('Streaming response — content is shown in Gateway Response above')}
                    </span>
                  </div>
                </div>
              ) : null}
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
