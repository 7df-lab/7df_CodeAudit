// ADR-195 回归锁：AI 结论 Source→Sink 链路普适解析器。
// 主用例=人类指令实例原文（gw-0fb985799a00ab4ba3995b98-sbx-1 ai_reasoning，逐字）。
import { describe, expect, it } from 'vitest';
import { baseName, parseChain } from '../findings/chainParser';

const USER_CASE =
  '[DSH-sandbox] MqttHttpApiListener.java:49 `private final HttpFilter authFilter;` 由 Builder 传入且默认 null；config() 第 93-95 行 `if (authFilter != null) { httpRouter.filter(authFilter); }` —— 为 null 时整个 router 无任何认证。MqttServerCreator.java:594-596 `enableMqttHttpApi()` 使用 `MqttHttpApiListener.Builder::build`，不设置 authFilter；starter/mica-mqtt-server-spring-boot-starter MqttServerProperties.java:261-265 `HttpBasicAuth.enable` 默认 false，MqttServerConfiguration.java:172-174 仅在 enable 时调用 `builder.basicAuth(...)`。而 MqttHttpApi.java 暴露的端点可直接操纵 broker：publish（153-167）、deleteClients 踢人（379-389）、subscribe 注入订阅（222-241）、getClients 列出全部客户端（365-369）。';

describe('parseChain——人类指令实例原文', () => {
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
