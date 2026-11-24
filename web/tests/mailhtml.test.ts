import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  IMG_BLOCKED, blockImages, buildFrameDoc, frameCsp, stripMetaAndBase,
} from '../src/lib/mailhtml.ts'

// 取出 srcdoc 文档里 <body> 与 </body> 之间的正文
const bodyOf = (doc: string) => doc.slice(doc.indexOf('<body>') + 6, doc.lastIndexOf('</body>'))
const CSP_RE = /<meta http-equiv="Content-Security-Policy" content="([^"]*)">/
const cspOf = (doc: string) => {
  const m = CSP_RE.exec(doc)
  assert.ok(m, '文档应带 CSP meta')
  return m[1]
}
const count = (s: string, re: RegExp) => (s.match(re) || []).length

test('摘除各种写法的 meta 与 base，其余内容原样保留', () => {
  const src = [
    '<meta http-equiv="refresh" content="0;url=https://evil.example/">',
    "<META HTTP-EQUIV='Refresh' CONTENT='0; URL=//evil.example'>",
    '<meta\nhttp-equiv=refresh content=0>',
    '< meta http-equiv=refresh>',
    '<meta/http-equiv="refresh" content="1">',
    '<base href="https://evil.example/">',
    '<base target=_blank>',
  ].join('\n')
  const out = stripMetaAndBase(src)
  assert.equal(out.trim(), '')
  assert.equal(count(out, /<\s*\/?\s*(meta|base)\b/i), 0)

  // 正常内容不动：样式、正文、别的标签
  const doc = '<html><head><meta charset="utf-8"><style>p{color:red}</style></head>' +
    '<body><p>你好 &amp; 再见</p><a href="https://ok.example">link</a></body></html>'
  assert.equal(
    stripMetaAndBase(doc),
    '<html><head><style>p{color:red}</style></head>' +
    '<body><p>你好 &amp; 再见</p><a href="https://ok.example">link</a></body></html>')
})

test('属性值里带 > 的 meta 也不会残留成标签', () => {
  const out = stripMetaAndBase('<p>a</p><meta name="x" content="a>b"><p>c</p>')
  assert.equal(count(out, /<meta/i), 0)
  assert.ok(out.startsWith('<p>a</p>') && out.endsWith('<p>c</p>'))
})

test('屏蔽图片：src 全部摘除并加占位 alt', () => {
  const out = blockImages(
    '<img src="https://t.example/p.gif" width="1">' +
    "<img SRC='https://t.example/q.gif'>" +
    '<img\nsrc=https://t.example/r.gif>' +
    '<img srcset="https://t.example/s.png 1x">' +
    '<img src="data:image/gif;base64,R0lGODlhAQABAAAAACw=" alt="dot">')
  // 远程地址只剩 srcset 里那一处（没有 src 可摘，由 CSP 拦截加载）
  assert.equal(count(out, /t\.example/g), 1)
  assert.equal(count(out, /<img/g), 5)
  assert.equal(count(out, /\bsrc\s*=/gi), 0)
  // 每个 img 都带占位，且占位排在最前（HTML 取第一个同名属性，原邮件自带的 alt 压不过它）
  assert.equal(count(out, /<img alt="\[图片已屏蔽\]"/g), 5)
})

test('两种模式都带 CSP，且 CSP 不含会撑破 meta 属性的字符', () => {
  for (const loadImages of [false, true]) {
    const csp = frameCsp(loadImages)
    assert.ok(!csp.includes('"') && !csp.includes('>'), `CSP(${loadImages}) 不能含引号或 >`)
    // 每条指令都是"名字 + 至少一个值"
    for (const d of csp.split(';')) {
      const parts = d.trim().split(/\s+/)
      assert.ok(parts.length >= 2, `空指令: ${d}`)
      assert.match(parts[0], /^[a-z-]+$/)
    }
    assert.match(csp, /(?:^|; )default-src 'none'/)
    assert.match(csp, /form-action 'none'/)
    assert.match(csp, /base-uri 'none'/)
    // 没有任何指令给脚本放行（default-src 'none' 即拒绝脚本）
    assert.ok(!/(?:^|; )script-src/.test(csp))
    assert.ok(!/(?:^|; )frame-src/.test(csp))
  }
})

test('屏蔽图片时的 CSP：只许内联样式与 data: 图片', () => {
  const csp = frameCsp(false)
  assert.match(csp, /img-src data:/)
  assert.ok(!/https?:/.test(csp), '屏蔽模式不许任何网络来源')
})

test('允许图片时的 CSP：放开图片/音视频/字体，外链样式表仍拒绝', () => {
  const csp = frameCsp(true)
  assert.match(csp, /img-src https: http: data:/)
  assert.match(csp, /media-src https: http:/)
  assert.match(csp, /font-src https: data:/)
  // style-src 只有内联：没有来源，也就没有外链样式表
  assert.match(csp, /style-src 'unsafe-inline'(?:;|$)/)
})

test('buildFrameDoc：屏蔽模式', () => {
  const doc = buildFrameDoc(
    '<meta http-equiv="refresh" content="0;url=https://evil.example/">' +
    '<p>hi</p><img src="https://t.example/p.gif">', false)
  // charset 最先、CSP 在正文之前、正文在 body 里
  assert.ok(doc.startsWith('<!doctype html><html><head><meta charset="utf-8">'))
  assert.ok(doc.indexOf('Content-Security-Policy') < doc.indexOf('<body>'))
  assert.ok(doc.endsWith('</body></html>'))
  // 文档里只剩我们自己的两个 meta（charset + CSP）：正文的 meta refresh 已摘除
  assert.equal(count(doc, /<meta/gi), 2)
  assert.equal(cspOf(doc), frameCsp(false))
  const body = bodyOf(doc)
  assert.ok(body.includes('<p>hi</p>'))
  assert.ok(!body.includes('evil.example') && !body.includes('t.example'))
  assert.ok(body.includes(`alt="${IMG_BLOCKED}"`))
})

test('buildFrameDoc：允许加载图片时同样带 CSP、同样摘除 meta refresh', () => {
  const img = '<img src="https://cdn.example/photo.png" width="600">'
  const doc = buildFrameDoc(img +
    '<meta http-equiv="refresh" content="0;url=https://evil.example/">' +
    '<base href="https://evil.example/">', true)
  const csp = cspOf(doc)
  assert.equal(csp, frameCsp(true))
  assert.ok(csp.includes("default-src 'none'"), '放开图片不等于放开脚本')
  const body = bodyOf(doc)
  assert.ok(body.includes(img), '图片地址原样保留')
  assert.equal(count(body, /<meta|<base/gi), 0)
  assert.equal(count(body, /evil\.example/g), 0)
})

test('buildFrameDoc：恢复加载图片时从原始 HTML 重建，占位不残留', () => {
  const doc = buildFrameDoc('<img src="https://cdn.example/a.png">', true)
  assert.ok(!bodyOf(doc).includes(IMG_BLOCKED))
})
