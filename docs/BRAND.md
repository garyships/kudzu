# Kudzu brand and visual style

## Logo

A commit graph that grows a leaf. Files are in [`docs/assets/logo/`](assets/logo/). The mark is a single colour, so use `currentColor` or one of the brand greens.

## Social and marketing images: "cream minimal"

Use this for 小红书, X, video covers and subtitles, slides, and any image made to promote Kudzu. The website and README keep their own dark/light themes.

**Colours**

| Role | Hex |
|---|---|
| Background | `#F7F4EC` (cream) |
| Text | `#0F1411` |
| Secondary text | `#5D655F` |
| Divider | `#0F1411` at 14% opacity, 4 px |
| Accent: key numbers and labels | `#15803D` |
| Highlighter band behind key words | `#C6F23B`, from 58% to 94% of the line height |

**Inverted chip.** Use it at most once per image, for the single most important phrase: a `#0F1411` box with `#C6F23B` text.

**Type**

| Use | Font |
|---|---|
| Chinese headlines | Noto Sans SC 900 |
| Chinese body text | Noto Sans SC 700 |
| Latin words and numbers | Geist 800 |
| Small labels and page numbers | Geist Mono |

**Layout**

- **One idea per image.** A big headline, then at most two or three short lines or a three-item list separated by thin dividers.
- **Margins:** about 96 px on a 1080 px-wide canvas, with plenty of empty space.
- **Leave out:**
  - illustrations, stickers and emoji
  - drop shadows and rotated elements
  - background textures, dot grids and gradients
- **Logo:** small (about 44 px) at the top left of inner pages, with the page number top right in Geist Mono. **Never put the logo on the cover or first image.**
- **Screenshots:** dark terminal screenshots and screen recordings may sit on the cream page as they are.

**Sizes**

| Use | Size |
|---|---|
| 小红书 image | 1080×1440 (3:4) |
| Vertical video | 1080×1920 |
| X image | 1600×900 (16:9) |

**Subtitles**

- Black text on a cream rounded bar, with the most important words on the lime highlighter.
- No outlined text and no emoji stickers.

## Voice

- Plain language. Say "AI", not "agent", in Chinese social copy.
- Lead with the problem, then the fix.
- Explain git in one line the first time it appears.
- Anthropic appears only as evidence for the problem: "做 Claude 的公司 Anthropic 自己在博客里写过". It never appears as Kudzu's owner or endorser.
- End with one attribution line: "独立开源项目，架构基于 Anthropic 公开的 Scion 设计文章。"
