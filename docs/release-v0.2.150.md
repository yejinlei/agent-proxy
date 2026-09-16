# v0.2.150

## 主要变更

### END 日志新增 leaked_tools 字段：泄漏工具调用从"知道有泄漏"到"知道泄漏了什么"

Codex 的 `detectToolCallInText` 一直只回答"模型有没有把工具调用写成纯文本"，
回答是三种语法的描述串加 80 字符上下文。要看出模型到底想调哪个工具，得肉眼看上下文。

`extractLeakedToolNames` 从尾部 1500 字符窗口（与检测器同窗口）抽具体工具名，
END 与 END(err) 两条汇总行各带一个 `leaked_tools=[...]` 字段，一行 grep 定位。

三种语法，覆盖生产日志里出现过的全部形态：

- Codex：`<tool_call>` 后裸名 / 反引号 / 双引号
- o1 XML：`<arg_value>NAME</arg_value>` 取标签内容
- Anthropic：`<tool_use name="X">` 取 name 属性值

## 实现里踩掉的三个坑

**坑 1：终止字符判断条件写反。** 裸名分支的扫描写成"是终止字符时继续前进"，
循环永远推不过起始位置，切片恒空，`if end>start` 永远为假——
`<tool_call> apply_patch` 返回空列表。改为"不是终止字符时前进"。

**坑 2：闭合标签少了开头的尖括号。** 搜的是不带前导尖括号的形式，
所以抽出来的名字带着尾部尖括号（`"apply_patch" + `<``，空内容时是单个尖括号）。

**坑 3：语义错，不是代码错。** 第一版从 `<parameter>` 取名字，
但 `<parameter>` 装的是**参数名**（command / arg_key），不是工具名。
Anthropic 的工具名只在 `<tool_use name="X">` 的 name 属性里。

这条是被测试抓出来的：用例还写着旧的错误期望值，实现已经正确返回空，
于是失败。已替换为真实的 `<tool_use name="X">` 用例，并补三条负向用例锁住边界：
`<parameter> name="command"` 返回空、`name="call_id"` 返回空、
`</tool_use>` 不误判。扫描上的守卫条件（紧跟的字符必须是空格或尖括号结束）
正是挡住 `</tool_use>` 被当成开标签读进去的那道闸。

顺带确认一个容易张冠李戴的形态：`<parameter=command>` 这种等号式标签里装的是
参数名 command，不是工具名，所以不查。

## 约束

`@AI_GUARD: RESPONSES_TOOLCALL_IN_TEXT` 保持纯观测——提取结果只喂日志，
不生成 function_call item，不改写出站体。测试文件同样挂了这个 guard 标记。

## 测试

`TestExtractLeakedToolNames` 22 个用例全过（正例 3 语法 × 多变体 + 去重 +
负例 3 条 + 128 字符上限 + 尾部窗口边界）。`go build ./...` 与
`go vet` 干净通过。

## 二进制

- 6 个跨平台二进制（linux/darwin/windows × amd64/arm64）
- `sha256sums.txt` 用可移植裸文件名，`sha256sum -c` 直接可用

## 说明

本次版本号在二进制里（`main.version=v0.2.150`）并已提交到 git。
