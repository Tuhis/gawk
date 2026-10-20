//! The Open-source licences page (R72, docs/74 D3): the platform's embedded
//! notices ([`crate::shell::Platform::notices`]), read by
//! `gawk_engine::notices`, as the rows the page lists.

use gawk_engine::notices::Notices;

use crate::NoticeRow;

const HEADING: i32 = 0;
const PARAGRAPH: i32 = 1;
const PACKAGE: i32 = 2;
const TEXT: i32 = 3;
const SOURCE: i32 = 4;

fn row(kind: i32, title: &str, sub: &str, body: &str) -> NoticeRow {
    NoticeRow {
        kind,
        title: title.into(),
        sub: sub.into(),
        body: body.into(),
    }
}

/// The page, top to bottom: what the app is made of, every package with its
/// licence and copyright, where the MPL sources are, then the licence texts.
pub fn rows(n: &Notices) -> Vec<NoticeRow> {
    let mut out = vec![row(
        PARAGRAPH,
        "gawk is open source under the Apache License 2.0. It is built on the \
         open-source software below, each under its own licence.",
        "",
        "",
    )];
    out.extend(n.intro.split("\n\n").map(|p| row(PARAGRAPH, p, "", "")));
    out.push(row(
        HEADING,
        &format!("PACKAGES ({})", n.packages.len()),
        "",
        "",
    ));
    out.extend(n.packages.iter().map(|p| {
        let title = if p.version.is_empty() {
            p.name.clone()
        } else {
            format!("{} {}", p.name, p.version)
        };
        row(PACKAGE, &title, &p.license, &p.copyright.join("\n"))
    }));
    if !n.sources.is_empty() {
        out.push(row(HEADING, "SOURCE CODE", "", ""));
        out.push(row(
            PARAGRAPH,
            "These packages are under the Mozilla Public License 2.0. Their \
             source code, unmodified, is at the addresses below.",
            "",
            "",
        ));
        out.extend(
            n.sources
                .iter()
                .map(|s| row(SOURCE, &s.package, &s.url, "")),
        );
    }
    out.push(row(HEADING, "LICENCE TEXTS", "", ""));
    out.extend(
        n.texts
            .iter()
            .map(|t| row(TEXT, &t.license, &t.applies_to, &t.body)),
    );
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use gawk_engine::notices::parse;

    #[test]
    fn every_package_text_and_source_becomes_a_row() {
        let n = parse(include_str!("../../../../gawk-ios/THIRD-PARTY-NOTICES.md"));
        let rows = rows(&n);
        let count = |kind| rows.iter().filter(|r| r.kind == kind).count();
        assert_eq!(count(PACKAGE), n.packages.len());
        assert_eq!(count(TEXT), n.texts.len());
        assert_eq!(count(SOURCE), n.sources.len());
        assert!(count(SOURCE) > 0, "the iOS file has the uniffi sources");
        let opus = rows
            .iter()
            .find(|r| r.kind == PACKAGE && r.title == "libopus 1.3")
            .expect("libopus");
        assert_eq!(opus.sub, "BSD-3-Clause");
        assert!(opus.body.contains("Xiph.Org"));
    }

    #[test]
    fn a_file_without_sources_has_no_source_heading() {
        let rows = rows(&parse(include_str!("../../../THIRD-PARTY-NOTICES.md")));
        assert!(
            !rows
                .iter()
                .any(|r| r.kind == HEADING && r.title == "SOURCE CODE")
        );
        assert!(
            rows.iter()
                .any(|r| r.kind == HEADING && r.title.starts_with("PACKAGES ("))
        );
    }

    #[test]
    fn nothing_parsed_still_says_what_the_page_is() {
        let rows = rows(&Notices::default());
        assert_eq!(rows[0].kind, PARAGRAPH);
        assert!(rows.iter().any(|r| r.title == "PACKAGES (0)"));
    }
}
