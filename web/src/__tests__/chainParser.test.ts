// ADR-195 回归锁：AI 结论 Source→Sink 链路普适解析器。
// 主用例=真实实例原文（gw-0fb985799a00ab4ba3995b98-sbx-1 ai_reasoning，逐字）。
import { describe, expect, it } from 'vitest';
import { baseName, parseChain } from '../findings/chainParser';

const USER_CASE =
  '[DSH-sandbox] MqttHttpApiListener.java:49 `private final HttpFilter authFilter;` 由 Builder 传入且默认 null；config() 第 93-95 行 `if (authFilter != null) { httpRouter.filter(authFilter); }` —— 为 null 时整个 router 无任何认证。MqttServerCreator.java:594-596 `enableMqttHttpApi()` 使用 `MqttHttpApiListener.Builder::build`，不设置 authFilter；starter/mica-mqtt-server-spring-boot-starter MqttServerProperties.java:261-265 `HttpBasicAuth.enable` 默认 false，MqttServerConfiguration.java:172-174 仅在 enable 时调用 `builder.basicAuth(...)`。而 MqttHttpApi.java 暴露的端点可直接操纵 broker：publish（153-167）、deleteClients 踢人（379-389）、subscribe 注入订阅（222-241）、getClients 列出全部客户端（365-369）。';

describe('parseChain——真实实例原文', () => {
  const r = parseChain(USER_CASE);

  it('hops 按原文顺序：file:line → 中文行引用 → 括号区间（挂接最近文件）', () => {
    expect(r.hops.map((h) => [h.path, h.line, h.endLine])).toEqual([
      ['MqttHttpApiListener.java', 49, undefined],
      ['MqttHttpApiListener.java', 93, 95],
      ['MqttServerCreator.java', 594, 596],
      ['MqttServerProperties.java', 261, 265],
      ['MqttServerConfiguration.java', 172, 174],
      ['MqttHttpApi.java', 153, 167],
      ['MqttHttpApi.java', 379, 389],
      ['MqttHttpApi.java', 222, 241],
      ['MqttHttpApi.java', 365, 369],
    ]);
  });

  it('files：全部提及文件按首现顺序（含无行号的 MqttHttpApi.java）', () => {
    expect(r.files).toEqual([
      'MqttHttpApiListener.java',
      'MqttServerCreator.java',
      'MqttServerProperties.java',
      'MqttServerConfiguration.java',
      'MqttHttpApi.java',
    ]);
  });

  it('代码片段噪声不进文件表：Foo.Builder（大写扩展）/ httpRouter.filter(（调用形）/ starter 目录', () => {
    expect(r.files.some((f) => f.includes('Builder') || f.includes('filter') || f.includes('starter'))).toBe(false);
  });

  it('每跳携带原文片段供人工核对；端点关键词标注 sink', () => {
    for (const h of r.hops) expect(h.snippet.length).toBeGreaterThan(0);
    const sinkHops = r.hops.filter((h) => h.role === 'sink');
    expect(sinkHops.length).toBeGreaterThanOrEqual(4); // publish/deleteClients/subscribe/getClients（端点/暴露关键词）
    expect(sinkHops.every((h) => h.path === 'MqttHttpApi.java')).toBe(true);
  });
});

describe('parseChain——普适形态', () => {
  it('带路径前缀的 file:line（与发现 location 同形态）', () => {
    const r = parseChain('入口在 src/main/java/Foo.java:12，危险执行在 bar/baz.py:30-32');
    expect(r.hops).toEqual([
      expect.objectContaining({ path: 'src/main/java/Foo.java', line: 12 }),
      expect.objectContaining({ path: 'bar/baz.py', line: 30, endLine: 32 }),
    ]);
  });

  it('L 前缀 / lines 英文行引用挂接最近文件', () => {
    const r = parseChain('App.java 中 L49 未校验，lines 60-70 存在拼接');
    expect(r.hops).toEqual([
      expect.objectContaining({ path: 'App.java', line: 49 }),
      expect.objectContaining({ path: 'App.java', line: 60, endLine: 70 }),
    ]);
  });

  it('source/sink 关键词标注（来源→入口、汇点→危险）', () => {
    const r = parseChain('来源 App.py:10 用户输入，汇点 Run.py:99 危险执行');
    expect(r.hops[0].role).toBe('source');
    expect(r.hops[1].role).toBe('sink');
  });

  it('无关键词不标注 role（不推测）；空文本/无引用返回空', () => {
    expect(parseChain('App.py:1 简单描述').hops[0].role).toBeUndefined();
    expect(parseChain('')).toEqual({ hops: [], files: [] });
    expect(parseChain(null)).toEqual({ hops: [], files: [] });
    expect(parseChain('该发现无任何文件引用，仅文字描述').files).toEqual([]);
  });

  it('行引用先于任何文件提及时丢弃（无挂接对象）；IP/版本号不是文件', () => {
    expect(parseChain('第 5 行有问题').hops).toEqual([]);
    const r = parseChain('升级到 1.2.1 后访问 gateway.internal:8080 出错，App.go:9 崩溃');
    expect(r.files).toEqual(['App.go']);
    expect(r.hops).toEqual([expect.objectContaining({ line: 9 })]);
  });

  it('真实调用括号（含标识符参数）不误判为行区间', () => {
    const r = parseChain('Config.java:8 调用 filter(authFilter) 失败');
    expect(r.hops).toEqual([expect.objectContaining({ path: 'Config.java', line: 8 })]);
  });

  it('同一 file:line 重复引用去重', () => {
    const r = parseChain('A.py:1 x；再次强调 A.py:1');
    expect(r.hops.length).toBe(1);
  });
});

describe('baseName', () => {
  it('取末段', () => {
    expect(baseName('a/b/c.java')).toBe('c.java');
    expect(baseName('c.java')).toBe('c.java');
  });
});

// ---- 两阶段·存在性校验（2026-09-13 误挂接根治，gw-7c26f71c1771c76444baddd0-sbx-14 实证）----
// 真实语料：AI 原文反复出现 C++ 成员表达式 new_size.x（形似文件名），且对真实文件
// RafDecoder.cpp 写裸行引用 (line 86)/(lines 213-218)——旧解析把裸行引用挂到 new_size.x
// 上，产出指向不存在文件的 chip（全文复核 404 降级）。
const SBX14_CASE =
  '[DSH-sandbox] Verified RafDecoder.cpp:247-265: the guard tests only `h < rotated->dim.y && w < rotated->dim.x`; ' +
  'with alt_layout and odd new_size.x, h = new_size.x/2 - 1 + y - (x>>1) evaluates to -1 at (0, new_size.x-1) ' +
  '(integer division of odd n: n/2 == (n-1)/2), and dst[w + h*dest_pitch] then targets one row before the buffer start. ' +
  'alt_layout is set from the FUJI_LAYOUT tag high bit (line 86). The relative-crop branch (lines 213-218) derives ' +
  'new_size from attacker-controlled mRaw->dim; hints.has("fuji_rotate") comes from the camera DB, so the loop is ' +
  'gated on a populated DB - hence production-only severity.';

describe('parseChain 两阶段——sbx-14 真实语料（new_size.x 误挂根治）', () => {
  const existsMock = (p: string) => (p === 'RafDecoder.cpp' ? true : p === 'new_size.x' ? false : undefined);

  it('无 opts 调用 = 旧行为逐字节等价（回归锚）：裸行引用误挂 new_size.x', () => {
    const r = parseChain(SBX14_CASE);
    expect(r.hops.map((h) => [h.path, h.line, h.endLine])).toEqual([
      ['RafDecoder.cpp', 247, 265],
      ['new_size.x', 86, undefined],
      ['new_size.x', 213, 218],
    ]);
    expect(r.files).toEqual(['RafDecoder.cpp', 'new_size.x']);
  });

  it('注入 fileExists：假文件 token 摘除，裸行引用重挂最近有效文件并标 inferred', () => {
    const r = parseChain(SBX14_CASE, { fileExists: existsMock });
    expect(r.hops.map((h) => [h.path, h.line, h.endLine])).toEqual([
      ['RafDecoder.cpp', 247, 265],
      ['RafDecoder.cpp', 86, undefined],
      ['RafDecoder.cpp', 213, 218],
    ]);
    // 显式引用（第 1 跳）不标 inferred；裸行引用（2/3 跳）统一标推断
    expect(r.hops[0].inferred).toBeUndefined();
    expect(r.hops[1].inferred).toBe(true);
    expect(r.hops[2].inferred).toBe(true);
    expect(r.files).toEqual(['RafDecoder.cpp']); // new_size.x 不进文件表/下拉
  });

  it('fileExists 全 undefined（探测未返回）→ fail-open 等价旧行为', () => {
    const r = parseChain(SBX14_CASE, { fileExists: () => undefined, fallbackFile: 'RafDecoder.cpp' });
    expect(r.hops.map((h) => [h.path, h.line, h.endLine])).toEqual(
      parseChain(SBX14_CASE).hops.map((h) => [h.path, h.line, h.endLine]),
    );
  });
});

describe('parseChain 两阶段——幻觉引用/兜底挂接', () => {
  it('显式 file:line 指向不存在文件：保留 hop 标 unresolved，不进 files（幻觉可见）', () => {
    const r = parseChain('Config.java:8 入口；另见 Ghost.py:12 数据流', {
      fileExists: (p) => (p === 'Config.java' ? true : p === 'Ghost.py' ? false : undefined),
    });
    expect(r.hops).toHaveLength(2);
    expect(r.hops[0]).toMatchObject({ path: 'Config.java', line: 8 });
    expect(r.hops[0].unresolved).toBeUndefined();
    expect(r.hops[1]).toMatchObject({ path: 'Ghost.py', line: 12, unresolved: true });
    expect(r.files).toEqual(['Config.java']);
  });

  it('无行号的假文件 token 不产 hop、不抢挂接语境', () => {
    const r = parseChain('RafDecoder.cpp:10 讨论到 new_size.x 之后的 (line 5) 发生越界', {
      fileExists: (p) => (p === 'RafDecoder.cpp' ? true : p === 'new_size.x' ? false : undefined),
    });
    expect(r.hops).toEqual([
      expect.objectContaining({ path: 'RafDecoder.cpp', line: 10 }),
      expect.objectContaining({ path: 'RafDecoder.cpp', line: 5, inferred: true }),
    ]);
  });

  it('全文无有效文件 token：裸行引用兜底挂 fallbackFile（漏洞所在文件）', () => {
    const r = parseChain('校验缺失处 (line 42) 直接写出', { fallbackFile: 'Vuln.py' });
    expect(r.hops).toEqual([expect.objectContaining({ path: 'Vuln.py', line: 42, inferred: true })]);
  });

  it('全部 token 不存在且无 fallback：显式引用保留 unresolved，裸行引用丢弃', () => {
    const r = parseChain('Foo.rb:3 x；随后 (line 7) 崩溃', { fileExists: () => false });
    expect(r.hops).toEqual([expect.objectContaining({ path: 'Foo.rb', line: 3, unresolved: true })]);
    expect(r.files).toEqual([]);
  });
});
