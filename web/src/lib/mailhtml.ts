// 邮件 HTML 正文放进 sandbox iframe 之前的预处理。纯函数，不碰 DOM——可以直接用 node --test 单测。
//
// 正文是不可信的第三方内容。sandbox="" 挡住了脚本、表单、弹窗和顶层跳转，但框架**自身**的跳转
// 不受限：规范上正文里的 <meta http-equiv="refresh"> 可以让 iframe 自己走到别的地址，而 CSP 没有
// 指令能管它，所以从源头把正文里的 meta 摘掉（<base> 会改写相对地址的解析基准，一并去掉）。
// 实测 Chrome 157 里 sandbox 属性本身就会拦下标记内的 meta refresh，但那不是跨浏览器的保证，
// 不拿它当防线。
// CSP 则始终带上："加载远程图片"放开的是图片等子资源，不是脚本、框架、表单、外链样式表。
// （不带 CSP 的话，允许加载图片后这些都会照常加载。）

// 正文里的 <meta> / <base> 一律摘除（大小写不敏感；"< meta" 这类写法也算，宁可多摘）。
// <base> 会改写正文里相对地址的解析基准，一并去掉。
const META_OR_BASE = /<\s*(?:meta|base)\b[^>]*>/gi

// 屏蔽远程图片时的占位提示
export const IMG_BLOCKED = '[图片已屏蔽]'

// 屏蔽图片：除内联样式与 data: 图片外不许任何加载（含无引号 src、srcset、CSS url()）。
const CSP_BLOCKED = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; " +
  "base-uri 'none'; form-action 'none'"
// 允许图片：只放开图片/音视频/字体，其余照旧拒绝——尤其是脚本（style-src 不含任何来源，
// 外链样式表也挡在外面，它同样是追踪手段）。
const CSP_IMAGES = "default-src 'none'; img-src https: http: data:; media-src https: http:; " +
  "font-src https: data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

export function frameCsp(loadImages: boolean): string {
  return loadImages ? CSP_IMAGES : CSP_BLOCKED
}

/** 摘除正文里的 <meta> 与 <base> 标签。 */
export function stripMetaAndBase(html: string): string {
  return html.replace(META_OR_BASE, '')
}

/**
 * 摘除 <img> 的 src 并加上占位 alt。只用于屏蔽图片的展示；恢复加载时用原始 HTML 重建
 * （不在这个结果上"恢复"）。
 */
export function blockImages(html: string): string {
  return html
    .replace(/<img\b([^>]*)\bsrc\s*=\s*("([^"]*)"|'([^']*)'|[^\s>]+)/gi, '<img$1')
    .replace(/<img\b([^>]*?)\/?>/gi, (m) => m.replace(/<img\b/i, `<img alt="${IMG_BLOCKED}"`))
}

/** 组装 iframe 的 srcdoc 文档：自带 charset 与 CSP，正文放在其中。 */
export function buildFrameDoc(bodyHtml: string, loadImages: boolean): string {
  const html = loadImages ? stripMetaAndBase(bodyHtml) : blockImages(stripMetaAndBase(bodyHtml))
  return `<!doctype html><html><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="${frameCsp(loadImages)}">
<style>body{font-family:inherit;margin:8px;color:#333;word-break:break-word}img{max-width:100%}</style>
</head><body>${html}</body></html>`
}
