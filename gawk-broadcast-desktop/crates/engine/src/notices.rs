//! The third-party notices, read for display (R72, docs/74 D2).
//!
//! Every native app embeds its own generated `THIRD-PARTY-NOTICES*.md` and
//! shows it under Settings → Open-source licences, so the attributions travel
//! inside the binary rather than beside it. The shell picks the file
//! (`include_str!`) the way it picks its identity; this reads it. The input
//! is `tools/licenses/gen-notices.py`'s own output, not Markdown at large:
//! the title, the blurb, the Packages table, an optional Source code table
//! and the License texts, each text a `###` heading, an "Applies to:" line
//! and one fenced block. The tests parse every committed file, so a change
//! to the generator's layout fails here before it ships an empty page.

/// One dependency: a row of the Packages table.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Package {
    pub name: String,
    pub version: String,
    /// The licence as declared (an SPDX expression, as the table has it).
    pub license: String,
    /// The copyright lines; empty when the package names none.
    pub copyright: Vec<String>,
}

/// Where an MPL package's source can be had (MPL-2.0 §3.2(a)).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Source {
    /// The package and its version, as the table has them: `uniffi 0.32.2`.
    pub package: String,
    pub url: String,
}

/// One distinct licence text and the packages it covers.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LicenseText {
    /// The licence's label: `MIT`, `Apache-2.0 OR MIT`.
    pub license: String,
    /// The packages it applies to, as the file lists them (the first twelve
    /// and "and N more"); every package's own licence is in its row.
    pub applies_to: String,
    pub body: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Notices {
    /// The component the file is for: `gawk-broadcast-macos`.
    pub component: String,
    /// The generator's blurb, as plain text: what ships, and the entries that
    /// deserve a second look.
    pub intro: String,
    pub packages: Vec<Package>,
    pub sources: Vec<Source>,
    pub texts: Vec<LicenseText>,
}

#[derive(PartialEq)]
enum Section {
    Intro,
    Other,
    Packages,
    Sources,
    Texts,
}

/// Reads a generated notices file. Lines it does not recognise are skipped,
/// never an error: the page shows what parsed.
pub fn parse(md: &str) -> Notices {
    let mut out = Notices::default();
    let mut section = Section::Other;
    let mut intro: Vec<&str> = Vec::new();
    let mut fence: Option<Vec<&str>> = None;
    let mut heading: Option<(String, String)> = None;

    for line in md.lines() {
        // Inside a licence text everything is body, including the `##`
        // headings some licences carry (Slint's, the Apache appendix).
        if let Some(body) = fence.as_mut() {
            if line == "```" {
                let (license, applies_to) = heading.take().unwrap_or_default();
                out.texts.push(LicenseText {
                    license,
                    applies_to,
                    body: body.join("\n"),
                });
                fence = None;
            } else {
                body.push(line);
            }
            continue;
        }
        if let Some(rest) = line.strip_prefix("# ") {
            out.component = rest
                .rsplit('—')
                .next()
                .unwrap_or(rest)
                .trim()
                .trim_matches('`')
                .to_owned();
            section = Section::Intro;
            continue;
        }
        if let Some(rest) = line.strip_prefix("## ") {
            section = match rest.trim() {
                "Packages" => Section::Packages,
                "Source code" => Section::Sources,
                "License texts" => Section::Texts,
                _ => Section::Other,
            };
            continue;
        }
        match section {
            Section::Intro => {
                if line.starts_with("This file is generated") {
                    section = Section::Other;
                } else {
                    intro.push(line);
                }
            }
            Section::Packages => {
                if let [name, version, license, copyright] = cells(line).as_slice()
                    && name.starts_with('`')
                {
                    out.packages.push(Package {
                        name: unquote(name),
                        version: dash_empty(version),
                        license: unquote(license),
                        copyright: copyright
                            .split("<br>")
                            .map(str::trim)
                            .filter(|c| !c.is_empty() && *c != "—")
                            .map(str::to_owned)
                            .collect(),
                    });
                }
            }
            Section::Sources => {
                if let [package, url] = cells(line).as_slice()
                    && package.starts_with('`')
                {
                    out.sources.push(Source {
                        package: package.replace('`', ""),
                        url: url.trim_matches(['<', '>']).to_owned(),
                    });
                }
            }
            Section::Texts => {
                if let Some(rest) = line.strip_prefix("### ") {
                    // "12. Apache-2.0 OR MIT — 3 packages"
                    let label = rest.split_once(". ").map_or(rest, |(_, l)| l);
                    let license = label.rsplit_once(" — ").map_or(label, |(l, _)| l);
                    heading = Some((license.trim().to_owned(), String::new()));
                } else if let Some(rest) = line.strip_prefix("Applies to: ") {
                    if let Some((_, applies)) = heading.as_mut() {
                        *applies = rest.replace('`', "");
                    }
                } else if line == "```" {
                    fence = Some(Vec::new());
                }
            }
            Section::Other => {}
        }
    }
    out.intro = plain(&intro.join("\n"));
    out
}

/// A table row's cells, or nothing for a line that is not one.
fn cells(line: &str) -> Vec<&str> {
    let Some(inner) = line.strip_prefix("| ").and_then(|l| l.strip_suffix(" |")) else {
        return Vec::new();
    };
    inner.split(" | ").map(str::trim).collect()
}

fn unquote(cell: &str) -> String {
    cell.trim_matches('`').to_owned()
}

fn dash_empty(cell: &str) -> String {
    if cell == "—" {
        String::new()
    } else {
        cell.to_owned()
    }
}

/// The blurb's Markdown as text a label can show: paragraphs kept, lines
/// within one joined, bold and code marks dropped.
fn plain(md: &str) -> String {
    md.split("\n\n")
        .map(|p| p.split_whitespace().collect::<Vec<_>>().join(" "))
        .filter(|p| !p.is_empty())
        .map(|p| p.replace("**", "").replace('`', ""))
        .collect::<Vec<_>>()
        .join("\n\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = "# Third-party notices — `gawk-ios`

The gawk iOS app. **libopus** is compiled in
through `audiopus_sys`.

Second paragraph.

This file is generated. Do not edit it by hand — run

**Scope:** something

## Summary — 2 packages

| License (as declared) | Packages |
|---|---:|
| `MIT` | 1 |

## Packages

| Package | Version | License | Copyright |
|---|---|---|---|
| `libopus` | 1.3 | `BSD-3-Clause` | Copyright 2001-2011 Xiph.Org<br>Copyright 2012 Someone |
| `uniffi` | 0.32.2 | `MPL-2.0` | — |

## Source code

These packages are distributed under the Mozilla Public License 2.0.

| Package | Source |
|---|---|
| `uniffi` 0.32.2 | <https://crates.io/crates/uniffi/0.32.2> |

## License texts

Each distinct license text below appears once.

### 1. BSD-3-Clause — 1 package

Applies to: `libopus`

```
Copyright 2001-2011 Xiph.Org

## Not a section
Redistribution and use
```

### 2. MPL-2.0 — 1 package

Applies to: `uniffi`, `uniffi_core` — and 4 more

```
Mozilla Public License Version 2.0
```
";

    #[test]
    fn reads_every_part_of_a_notices_file() {
        let n = parse(SAMPLE);
        assert_eq!(n.component, "gawk-ios");
        assert_eq!(
            n.intro,
            "The gawk iOS app. libopus is compiled in through audiopus_sys.\n\nSecond paragraph."
        );
        assert_eq!(
            n.packages,
            vec![
                Package {
                    name: "libopus".into(),
                    version: "1.3".into(),
                    license: "BSD-3-Clause".into(),
                    copyright: vec![
                        "Copyright 2001-2011 Xiph.Org".into(),
                        "Copyright 2012 Someone".into()
                    ],
                },
                Package {
                    name: "uniffi".into(),
                    version: "0.32.2".into(),
                    license: "MPL-2.0".into(),
                    copyright: vec![],
                },
            ]
        );
        assert_eq!(
            n.sources,
            vec![Source {
                package: "uniffi 0.32.2".into(),
                url: "https://crates.io/crates/uniffi/0.32.2".into(),
            }]
        );
        assert_eq!(n.texts.len(), 2);
        assert_eq!(n.texts[0].license, "BSD-3-Clause");
        assert_eq!(n.texts[0].applies_to, "libopus");
        // A heading inside a licence is the licence's, not a new section.
        assert_eq!(
            n.texts[0].body,
            "Copyright 2001-2011 Xiph.Org\n\n## Not a section\nRedistribution and use"
        );
        assert_eq!(n.texts[1].license, "MPL-2.0");
        assert_eq!(n.texts[1].applies_to, "uniffi, uniffi_core — and 4 more");
    }

    #[test]
    fn nothing_recognisable_is_an_empty_page_not_a_panic() {
        assert_eq!(parse(""), Notices::default());
        assert!(parse("| a | b |\n```\nunterminated").texts.is_empty());
    }

    /// The summary's count, which the generator writes from the same list.
    fn declared_count(md: &str) -> usize {
        md.lines()
            .find_map(|l| l.strip_prefix("## Summary — "))
            .and_then(|r| r.split_whitespace().next())
            .and_then(|n| n.parse().ok())
            .expect("a summary line")
    }

    /// Every file an app embeds parses whole: each package, a licence for
    /// each, the texts with bodies, and the C libraries the generator adds.
    fn check_committed(md: &str, component: &str, natives: &[&str]) {
        let n = parse(md);
        assert_eq!(n.component, component);
        assert!(!n.intro.is_empty());
        assert_eq!(
            n.packages.len(),
            declared_count(md),
            "{component}: every row"
        );
        assert!(
            n.packages
                .iter()
                .all(|p| !p.name.is_empty() && !p.license.is_empty())
        );
        assert!(n.texts.len() > 10, "{component}: the licence texts");
        assert!(n.texts.iter().all(|t| !t.license.is_empty()
            && !t.applies_to.is_empty()
            && !t.body.trim().is_empty()));
        for native in natives {
            assert!(
                n.packages.iter().any(|p| p.name == *native),
                "{component}: {native}"
            );
            assert!(
                n.texts
                    .iter()
                    .any(|t| t.applies_to.split(", ").any(|a| a == *native)),
                "{component}: {native}'s text"
            );
        }
    }

    #[test]
    fn the_committed_windows_notices_parse() {
        check_committed(
            include_str!("../../../THIRD-PARTY-NOTICES.md"),
            "gawk-broadcast-desktop",
            &["libopus"],
        );
    }

    #[test]
    fn the_committed_macos_notices_parse() {
        check_committed(
            include_str!("../../../THIRD-PARTY-NOTICES-macos.md"),
            "gawk-broadcast-macos",
            &["libopus"],
        );
    }

    #[test]
    fn the_committed_linux_notices_parse() {
        check_committed(
            include_str!("../../../THIRD-PARTY-NOTICES-linux.md"),
            "gawk-broadcast-linux",
            &["libopus"],
        );
    }

    #[test]
    fn the_committed_ios_notices_parse() {
        let md = include_str!("../../../../gawk-ios/THIRD-PARTY-NOTICES.md");
        check_committed(md, "gawk-ios", &["libopus", "libvpx"]);
        // MPL-2.0 §3.2(a): the uniffi crates say where their source is.
        let n = parse(md);
        assert!(
            n.sources
                .iter()
                .any(|s| s.package.starts_with("uniffi ") && s.url.starts_with("https://"))
        );
    }
}
