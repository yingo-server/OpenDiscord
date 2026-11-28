# UFOT 品牌规范（精简）

## 名称

- **全称**：Unfettered Freedom Of Thought
- **简称 / 标识**：UFOT（全大写，四个字母，不做大小写变体）
- **口号**："They demanded unfettered freedom of thought. / 他们要求不受束缚的思想自由。"
  这句话是项目名的来源，也是官网唯一文案；不在产品 UI、命令行输出或代码中重复出现。

## 视觉基调

- **底色**：近黑 `#0a0a0a`（官网、字标）；文档保持常规浅底，不套黑底。
- **文字**：纯白 `#fff` 主体，次要信息浅灰 `#cfcfcf`；无彩色强调色。
- **字体**：英文衬线（Times New Roman / Georgia）；中文简宋（Noto Serif SC 子集，
  官网仅裁切标语 14 字，矢量 woff2）。产品 UI 仍用各自现有字体栈，不强行统一到衬线。
- **风格**：电影感、克制、留白。不使用渐变、emoji、图标徽章、发光边框等装饰。
- **effect**：官网文字允许极轻辉光（text-shadow 低透明度），禁止高亮霓虹。

## 资产

| 文件 | 用途 |
|---|---|
| `brand/ufot-wordmark.svg` | 字标：UFOT + 细分割线 + 全称，深色底、单色、矢量 |
| `brand/favicon.svg` | 站点图标：黑底白字 U + 细线，矢量 |
| `index.html` 配套 `night-bg.jpg` / `notoserif-subset.woff2` | 官网唯一组合，不拆用 |

资产均为矢量或单色，便于任意缩放与再着色；修改需保持"名称+全称+细线"结构，
不增加图形符号。

## 使用规则

- README/文档引用字标用 `<img width="320">`，不改色、不拉伸。
- 命令行输出、systemd 单元、配置文件只写 `UFOT`，不写全称、不加口号。
- Logo 与产品名称不可分割使用；不得将 UFOT 字样与第三方品牌并列暗示关联。
- 旧名 OpenDiscord 仅存于历史 tag/release 资产名中（历史事实，不修改）；
  新文档、新代码一律使用 UFOT。
