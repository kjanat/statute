use lol_html::{HtmlRewriter, Settings, element, end_tag};
use std::cell::{Cell, RefCell};
use std::rc::Rc;

fn end_events(input: &[u8], chunk: usize) -> Vec<(usize, String, String)> {
    let events = Rc::new(RefCell::new(Vec::new()));
    let captured = Rc::clone(&events);
    let next_id = Cell::new(0);
    let settings =
        Settings::new().append_element_content_handler(element!("div, span, br", move |el| {
            let id = next_id.get() + 1;
            next_id.set(id);
            if !el.can_have_content() {
                return Ok(());
            }
            let start_name = el.tag_name();
            let captured = Rc::clone(&captured);
            el.on_end_tag(end_tag!(move |end| {
                captured
                    .borrow_mut()
                    .push((id, start_name.clone(), end.name()));
                Ok(())
            }))?;
            Ok(())
        }));
    let mut rewriter = HtmlRewriter::new(settings, |_: &[u8]| {});
    for bytes in input.chunks(chunk) {
        rewriter.write(bytes).unwrap();
    }
    rewriter.end().unwrap();
    Rc::try_unwrap(events).unwrap().into_inner()
}

// These assertions characterize the pinned dependency, including its mismatch
// with the documented missing-end contract. They do not select Statute's ABI.
#[test]
fn pinned_end_callback_identity() {
    type ExpectedEvent<'a> = (usize, &'a str, &'a str);
    type Case<'a> = (&'a [u8], &'a [ExpectedEvent<'a>]);
    let cases: &[Case<'_>] = &[
        (
            b"<div><span>text</span></div>",
            &[(2, "span", "span"), (1, "div", "div")],
        ),
        (
            b"<div><span>text</div>",
            &[(2, "span", "div"), (1, "div", "div")],
        ),
        (b"<div><span>text", &[]),
        (b"<div><br>text</div>", &[(1, "div", "div")]),
        (b"<div><div>text</div>", &[(2, "div", "div")]),
        (
            b"<div><span>text</aside></div>",
            &[(2, "span", "div"), (1, "div", "div")],
        ),
    ];
    for (input, expected) in cases {
        for chunk in [1, 4, input.len()] {
            let actual = end_events(input, chunk);
            let actual: Vec<_> = actual
                .iter()
                .map(|(id, start, end)| (*id, start.as_str(), end.as_str()))
                .collect();
            assert_eq!(&actual, expected, "input={input:?}, chunk={chunk}");
        }
    }
}

#[test]
fn implicit_child_end_callback_can_remove_ancestor_tag() {
    for chunk in [1, 4, 4096] {
        let mut output = Vec::new();
        let settings = Settings::new().append_element_content_handler(element!("span", |el| {
            el.on_end_tag(end_tag!(|end| {
                end.remove();
                Ok(())
            }))?;
            Ok(())
        }));
        let mut rewriter =
            HtmlRewriter::new(settings, |bytes: &[u8]| output.extend_from_slice(bytes));
        for bytes in b"<div><span>text</div>".chunks(chunk) {
            rewriter.write(bytes).unwrap();
        }
        rewriter.end().unwrap();
        assert_eq!(output, b"<div><span>text", "chunk={chunk}");
    }
}
