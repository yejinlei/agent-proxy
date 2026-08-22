## v0.2.96

### 修复

**文档站点 Mermaid 流程图在位渲染（md-viewer.html）**

- `docs/md-viewer.html`：`renderMermaid(md)` 改用 `querySelectorAll('pre > code.language-mermaid')` 捕获正文内的原始代码块，`code.parentElement.replaceWith(div)` **就地替换**为渲染后的 SVG，而非全部追加到 `#content` 末尾
- 修复前：16 张 SVG 全部堆在页面底部（DOM 索引 171–186），正文各章节仍是原始文本代码块
- 修复后：每张流程图出现在其对应章节下方（如 "Central Schema（翻译中枢）" 标题下），原始代码块零残留
- 同一修复同时覆盖 `DESIGN.md`（16 张图）、`ARCHITECTURE.md`（7 张图）及所有 `.md` 文件的 Mermaid 渲染
- 失败时显示红色错误横幅 + `console.error`，并回退显示原始源码

### 影响
- 纯前端文档渲染修复，无服务端行为变更
- GitHub Pages 站点（yejinlei.github.io/agent-proxy）自动生效