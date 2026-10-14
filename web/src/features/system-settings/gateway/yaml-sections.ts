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

// Pure helpers that split a gateway routing YAML document into top-level
// sections and join them back, so the UI can edit one category tab at a time
// while operating on the whole document. Slicing is char-offset based to keep
// comments, blank lines and indentation byte-for-byte identical (split/join is
// a round-trip identity), which keeps the Monaco editor cursor stable.

export type Section = {
  key: string
  text: string
}

export type TabDef = {
  value: string
  label: string
}

// Canonical top-level key order, matching service/gateway/types.go GatewayYaml.
export const KNOWN_KEYS = [
  'llm_gateway',
  'providers',
  'cost',
  'alarm',
  'model_name_mapping',
] as const

const TAB_LABELS: Record<string, string> = {
  llm_gateway: 'Routing & Groups',
  providers: 'Providers',
  cost: 'Cost',
  alarm: 'Alarm',
  model_name_mapping: 'Model Mapping',
}

const FULL_TAB = '__full__'
const OTHER_TAB = '__other__'

const TOP_LEVEL_KEY = /^[A-Za-z_][A-Za-z0-9_-]*:/

/**
 * Char offsets of each top-level key line (column 0, not a comment, not blank,
 * matching `identifier:`). Comments and blank lines at column 0 are not
 * boundaries — they attach to the key that follows.
 */
function topLevelKeyOffsets(text: string): { key: string; start: number }[] {
  const offsets: { key: string; start: number }[] = []
  let i = 0
  const len = text.length
  while (i < len) {
    // start of a line
    const lineEnd = text.indexOf('\n', i)
    const lineEndIdx = lineEnd === -1 ? len : lineEnd
    const line = text.slice(i, lineEndIdx)
    const isColumn0 = line.length === 0 || !/^\s/.test(line)
    if (isColumn0 && line.length > 0 && !line.startsWith('#')) {
      const match = TOP_LEVEL_KEY.exec(line)
      if (match) {
        const key = match[0].slice(0, -1) // strip trailing colon
        offsets.push({ key, start: i })
      }
    }
    i = lineEnd === -1 ? len : lineEnd + 1
  }
  return offsets
}

/**
 * Split a YAML document into top-level sections, in document order. A section's
 * text spans from its key line to the start of the next top-level key (or EOF),
 * so leading comments/blank lines before the first key attach to that first key.
 * Known keys absent from the document simply produce no section.
 */
export function splitYaml(text: string): Section[] {
  if (!text) return []
  const offsets = topLevelKeyOffsets(text)
  const sections: Section[] = []
  for (let i = 0; i < offsets.length; i++) {
    // The first section starts at the very top so any leading comments/blank
    // lines attach to the first key instead of being dropped (round-trip safe).
    const start = i === 0 ? 0 : offsets[i].start
    const end = i + 1 < offsets.length ? offsets[i + 1].start : text.length
    sections.push({ key: offsets[i].key, text: text.slice(start, end) })
  }
  return sections
}

/**
 * Join sections back into one document by plain concatenation. No formatting is
 * added or removed, so joinYaml(splitYaml(text)) === text.
 */
export function joinYaml(sections: Section[]): string {
  return sections.map((s) => s.text).join('')
}

/** Canonical rank of a key for insertion ordering (unknown keys sort last). */
function keyRank(key: string): number {
  const idx = KNOWN_KEYS.indexOf(key as (typeof KNOWN_KEYS)[number])
  return idx === -1 ? Number.MAX_SAFE_INTEGER : idx
}

/**
 * Replace a section's text if it exists, otherwise insert it at its canonical
 * position (before the first existing section with a higher rank). If the new
 * text is empty, an absent key stays absent and an existing key is removed,
 * so clearing a tab cleans the document rather than leaving a dangling entry.
 */
export function upsertSection(
  sections: Section[],
  key: string,
  text: string
): Section[] {
  const existingIdx = sections.findIndex((s) => s.key === key)
  if (text === '') {
    if (existingIdx === -1) return sections
    return sections.filter((s) => s.key !== key)
  }
  if (existingIdx !== -1) {
    return sections.map((s) => (s.key === key ? { key, text } : s))
  }
  const rank = keyRank(key)
  let insertAt = sections.length
  for (let i = 0; i < sections.length; i++) {
    if (keyRank(sections[i].key) > rank) {
      insertAt = i
      break
    }
  }
  const next = sections.slice()
  next.splice(insertAt, 0, { key, text })
  return next
}

/**
 * Tab list to render: all known keys always (so an absent category shows an
 * empty editor the user can type into), then an "Other" tab if any unknown
 * top-level keys exist, then "Full YAML" as the whole-document view.
 */
export function tabDefs(sections: Section[]): TabDef[] {
  const tabs: TabDef[] = KNOWN_KEYS.map((key) => ({
    value: key,
    label: TAB_LABELS[key] ?? key,
  }))
  if (
    sections.some(
      (s) => !KNOWN_KEYS.includes(s.key as (typeof KNOWN_KEYS)[number])
    )
  ) {
    tabs.push({ value: OTHER_TAB, label: 'Other' })
  }
  tabs.push({ value: FULL_TAB, label: 'Full YAML' })
  return tabs
}

export const FULL_TAB_VALUE = FULL_TAB
export const OTHER_TAB_VALUE = OTHER_TAB

/** The text shown in a tab's editor. */
export function tabText(sections: Section[], tabValue: string): string {
  if (tabValue === FULL_TAB) return joinYaml(sections)
  if (tabValue === OTHER_TAB) {
    return sections
      .filter((s) => !KNOWN_KEYS.includes(s.key as (typeof KNOWN_KEYS)[number]))
      .map((s) => s.text)
      .join('')
  }
  return sections.find((s) => s.key === tabValue)?.text ?? ''
}

/**
 * Produce the next sections state from an edit in a given tab. Known tabs
 * upsert that key; Other tab replaces all unknown sections with a re-split of
 * the typed text; Full tab re-splits the whole document.
 */
export function applyTabEdit(
  sections: Section[],
  tabValue: string,
  nextText: string
): Section[] {
  if (tabValue === FULL_TAB) {
    return splitYaml(nextText)
  }
  if (tabValue === OTHER_TAB) {
    const known = sections.filter((s) =>
      KNOWN_KEYS.includes(s.key as (typeof KNOWN_KEYS)[number])
    )
    const newUnknown = splitYaml(nextText).filter(
      (s) => !KNOWN_KEYS.includes(s.key as (typeof KNOWN_KEYS)[number])
    )
    return [...known, ...newUnknown]
  }
  return upsertSection(sections, tabValue, nextText)
}
