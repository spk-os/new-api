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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import {
  applyTabEdit,
  joinYaml,
  splitYaml,
  tabDefs,
  tabText,
  upsertSection,
} from './yaml-sections'

const SAMPLE = `# top-level comment
# belongs to first key
llm_gateway:
  default:
    group: default
providers:
  zhipu:
    base_url: https://open.bigmodel.cn
cost:
  ali_glm-5.2:
    input: 0.5
alarm:
  one-req-tokens: 200000
model_name_mapping:
  - pattern: Sonnet*
    replacement: ali_glm-5.2
# trailing comment after last key
`

const NO_ALARM = `llm_gateway:
  default:
    group: default
providers:
  zhipu:
    base_url: https://open.bigmodel.cn
cost:
  ali_glm-5.2:
    input: 0.5
model_name_mapping:
  - pattern: Sonnet*
    replacement: ali_glm-5.2
`

describe('yaml-sections round-trip', () => {
  test('joinYaml(splitYaml(text)) === text for a full config', () => {
    assert.equal(joinYaml(splitYaml(SAMPLE)), SAMPLE)
  })

  test('preserves comments, blank lines and indentation byte-for-byte', () => {
    const sections = splitYaml(SAMPLE)
    const llm = sections.find((s) => s.key === 'llm_gateway')
    assert.ok(llm)
    assert.ok(llm!.text.startsWith('# top-level comment'))
    assert.ok(llm!.text.includes('llm_gateway:'))
    // leading comments attach to the first key, not dropped
    assert.ok(sections[0].key === 'llm_gateway')
  })

  test('trailing comment after last key is preserved in last section', () => {
    const sections = splitYaml(SAMPLE)
    const last = sections[sections.length - 1]
    assert.equal(last.key, 'model_name_mapping')
    assert.match(last.text, /# trailing comment after last key\n?$/)
    assert.equal(joinYaml(sections), SAMPLE)
  })

  test('empty input yields no sections and joins to empty', () => {
    assert.deepEqual(splitYaml(''), [])
    assert.equal(joinYaml([]), '')
  })

  test('round-trip identity for config missing some known keys', () => {
    assert.equal(joinYaml(splitYaml(NO_ALARM)), NO_ALARM)
  })
})

describe('yaml-sections upsert', () => {
  test('inserts a missing known key at its canonical position', () => {
    const sections = splitYaml(NO_ALARM)
    const next = upsertSection(
      sections,
      'alarm',
      'alarm:\n  one-req-tokens: 200000\n'
    )
    const keys = next.map((s) => s.key)
    assert.deepEqual(keys, [
      'llm_gateway',
      'providers',
      'cost',
      'alarm',
      'model_name_mapping',
    ])
    // alarm sits between cost and model_name_mapping
    assert.equal(next[3].key, 'alarm')
    assert.equal(next[3].text, 'alarm:\n  one-req-tokens: 200000\n')
  })

  test('replaces text of an existing key without reordering', () => {
    const sections = splitYaml(SAMPLE)
    const next = upsertSection(
      sections,
      'cost',
      'cost:\n  ali_glm-5.2:\n    input: 1.0\n'
    )
    const cost = next.find((s) => s.key === 'cost')
    assert.equal(cost!.text, 'cost:\n  ali_glm-5.2:\n    input: 1.0\n')
    assert.deepEqual(
      next.map((s) => s.key),
      sections.map((s) => s.key)
    )
  })

  test('clearing a known key removes it; clearing an absent key is a no-op', () => {
    const sections = splitYaml(SAMPLE)
    const cleared = upsertSection(sections, 'alarm', '')
    assert.ok(!cleared.some((s) => s.key === 'alarm'))
    // absent key + empty text: no change
    const noop = upsertSection(splitYaml(NO_ALARM), 'alarm', '')
    assert.deepEqual(
      noop.map((s) => s.key),
      splitYaml(NO_ALARM).map((s) => s.key)
    )
  })
})

describe('yaml-sections tabs', () => {
  test('tabDefs lists all known keys always, plus Full at the end', () => {
    const tabs = tabDefs(splitYaml(NO_ALARM))
    assert.deepEqual(
      tabs.map((t) => t.value),
      [
        'llm_gateway',
        'providers',
        'cost',
        'alarm',
        'model_name_mapping',
        '__full__',
      ]
    )
  })

  test('tabDefs appends an Other tab when unknown top-level keys exist', () => {
    const withUnknown = `${NO_ALARM}custom_block:
  foo: bar
`
    const tabs = tabDefs(splitYaml(withUnknown))
    assert.deepEqual(
      tabs.map((t) => t.value),
      [
        'llm_gateway',
        'providers',
        'cost',
        'alarm',
        'model_name_mapping',
        '__other__',
        '__full__',
      ]
    )
  })

  test('tabText returns section text, empty string for absent key, joined for Full', () => {
    const sections = splitYaml(NO_ALARM)
    assert.equal(tabText(sections, 'alarm'), '')
    assert.ok(tabText(sections, 'cost').startsWith('cost:'))
    assert.equal(tabText(sections, '__full__'), joinYaml(sections))
  })

  test('Other tab text is the join of all unknown sections', () => {
    const text = `${NO_ALARM}custom_block:\n  foo: bar\n`
    const sections = splitYaml(text)
    assert.equal(tabText(sections, '__other__'), 'custom_block:\n  foo: bar\n')
  })
})

describe('yaml-sections applyTabEdit', () => {
  test('editing a known tab upserts that key', () => {
    const sections = splitYaml(NO_ALARM)
    const next = applyTabEdit(
      sections,
      'alarm',
      'alarm:\n  one-req-tokens: 200000\n'
    )
    assert.equal(
      next.find((s) => s.key === 'alarm')!.text,
      'alarm:\n  one-req-tokens: 200000\n'
    )
  })

  test('editing the Full tab re-splits the whole document', () => {
    const sections = splitYaml(SAMPLE)
    const fullText = tabText(sections, '__full__')
    // typing a char at the end: round-trip identity keeps value stable
    const next = applyTabEdit(sections, '__full__', fullText + '\n# note\n')
    assert.equal(joinYaml(next), fullText + '\n# note\n')
  })

  test('editing the Other tab replaces unknown sections only', () => {
    const text = `${NO_ALARM}custom_block:\n  foo: bar\n`
    const sections = splitYaml(text)
    const next = applyTabEdit(sections, '__other__', 'extra:\n  k: 1\n')
    assert.deepEqual(
      next.map((s) => s.key),
      ['llm_gateway', 'providers', 'cost', 'model_name_mapping', 'extra']
    )
  })
})
