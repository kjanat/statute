use lol_html::html_content::ContentType;
use lol_html::{HtmlRewriter, Settings, comments, element, end_tag, text};
use std::cell::{Cell, RefCell};
use std::rc::Rc;

#[test]
fn only_explicit_owner_receives_end_token() {
    type Event<'a> = (usize, &'a str, &'a str);
    type Case<'a> = (&'a str, &'a [Event<'a>]);
    let cases: &[Case<'_>] = &[
        (
            "<div><span>x</span></div>",
            &[(2, "span", "span"), (1, "div", "div")],
        ),
        ("<div><span>x</div>", &[(1, "div", "div")]),
        ("<div><span><b>x</div>", &[(1, "div", "div")]),
        ("<div><span>x", &[]),
        ("<div><br>x</div>", &[(1, "div", "div")]),
        ("<div><div>x</div>", &[(2, "div", "div")]),
        (
            "<div><div><span>x</div></div>",
            &[(2, "div", "div"), (1, "div", "div")],
        ),
        ("<div><span>x</aside></div>", &[(1, "div", "div")]),
        ("<DIV><SPAN>x</dIv>", &[(1, "div", "div")]),
        (
            "<svg><g><path/></g></svg>",
            &[(2, "g", "g"), (1, "svg", "svg")],
        ),
    ];
    for (input, expected) in cases {
        for chunk in [1, 4, input.len()] {
            let events = Rc::new(RefCell::new(Vec::new()));
            let captured = Rc::clone(&events);
            let next_id = Cell::new(0);
            let settings =
                Settings::new().append_element_content_handler(element!("*", move |el| {
                    let id = next_id.get() + 1;
                    next_id.set(id);
                    if el.can_have_content() {
                        let start = el.tag_name();
                        let captured = Rc::clone(&captured);
                        el.on_end_tag(end_tag!(move |end| {
                            captured.borrow_mut().push((id, start.clone(), end.name()));
                            Ok(())
                        }))?;
                    }
                    Ok(())
                }));
            let mut rewriter = HtmlRewriter::new(settings, |_: &[u8]| {});
            for bytes in input.as_bytes().chunks(chunk) {
                rewriter.write(bytes).unwrap();
            }
            rewriter.end().unwrap();
            let actual = Rc::try_unwrap(events).unwrap().into_inner();
            let actual: Vec<_> = actual
                .iter()
                .map(|(id, start, end)| (*id, start.as_str(), end.as_str()))
                .collect();
            assert_eq!(actual, *expected, "input={input}, chunk={chunk}");
        }
    }
}

#[test]
fn renamed_tokens_and_overlapping_handlers_keep_owner_and_order() {
    for chunk in [1, 4, 4096] {
        let events = Rc::new(RefCell::new(Vec::new()));
        let mut settings = Settings::new();
        for (rule, selector) in [(1, "div, span"), (2, "*")] {
            let events = Rc::clone(&events);
            settings = settings.append_element_content_handler(element!(selector, move |el| {
                let events = Rc::clone(&events);
                // Both original names become identical. Matching must still
                // use the parser's stack identity, not either output name.
                el.set_tag_name("section")?;
                el.on_end_tag(end_tag!(move |end| {
                    events.borrow_mut().push((rule, end.name()));
                    end.set_name("article");
                    Ok(())
                }))?;
                Ok(())
            }));
        }
        let mut output = Vec::new();
        let mut rewriter =
            HtmlRewriter::new(settings, |bytes: &[u8]| output.extend_from_slice(bytes));
        for bytes in b"<div><span>x</div>".chunks(chunk) {
            rewriter.write(bytes).unwrap();
        }
        rewriter.end().unwrap();
        assert_eq!(
            *events.borrow(),
            [(1, "section".into()), (2, "article".into())]
        );
        assert_eq!(output, b"<section><section>x</article>");
    }
}

struct Capture(Rc<Cell<usize>>);

impl Drop for Capture {
    fn drop(&mut self) {
        self.0.set(self.0.get() + 1);
    }
}

#[test]
fn implicit_handlers_drop_before_eof_without_an_ancestor_handler() {
    let dropped = Rc::new(Cell::new(0));
    let owner = Rc::clone(&dropped);
    let settings = Settings::new().append_element_content_handler(element!("span, b", move |el| {
        let capture = Capture(Rc::clone(&owner));
        el.on_end_tag(end_tag!(move |_| {
            let _keep_capture = &capture;
            panic!("implicit child received an ancestor token");
        }))?;
        Ok(())
    }));
    let mut rewriter = HtmlRewriter::new(settings, |_: &[u8]| {});
    for n in 1..=1024 {
        rewriter.write(b"<div><span><b>x</div>").unwrap();
        assert_eq!(
            dropped.get(),
            n * 2,
            "closures retained after subtree retirement"
        );
    }
    rewriter.end().unwrap();
    assert_eq!(Rc::strong_count(&dropped), 1);
}

#[test]
fn implicit_retirement_clears_text_scope_and_automatic_end_mutations() {
    for chunk in [1, 4, 4096] {
        let mut output = Vec::new();
        let settings = Settings::new()
            .append_element_content_handler(element!("span", |el| {
                el.append("must-not-appear", ContentType::Text);
                el.on_end_tag(end_tag!(|end| {
                    end.remove();
                    Ok(())
                }))?;
                Ok(())
            }))
            .append_element_content_handler(text!("span", |text| {
                text.replace("", ContentType::Text);
                Ok(())
            }))
            .append_element_content_handler(comments!("span", |comment| {
                comment.remove();
                Ok(())
            }));
        let mut rewriter =
            HtmlRewriter::new(settings, |bytes: &[u8]| output.extend_from_slice(bytes));
        for bytes in
            b"<div><span>inner<!--inside--></div>outside<!--outside--><p>after</p>".chunks(chunk)
        {
            rewriter.write(bytes).unwrap();
        }
        rewriter.end().unwrap();
        assert_eq!(
            output,
            b"<div><span></div>outside<!--outside--><p>after</p>"
        );
    }
}

#[test]
fn discarding_children_preserves_outer_handler_and_reuses_slots() {
    for chunk in [1, 4, 4096] {
        let events = Rc::new(RefCell::new(Vec::new()));
        let captured = Rc::clone(&events);
        let ids = Cell::new(0);
        let settings =
            Settings::new().append_element_content_handler(element!("div, span", move |el| {
                let id = ids.get() + 1;
                ids.set(id);
                let captured = Rc::clone(&captured);
                el.on_end_tag(end_tag!(move |_| {
                    captured.borrow_mut().push(id);
                    Ok(())
                }))?;
                Ok(())
            }));
        let mut rewriter = HtmlRewriter::new(settings, |_: &[u8]| {});
        for bytes in b"<div><section><span>x</section><span>y</span></div>".chunks(chunk) {
            rewriter.write(bytes).unwrap();
        }
        rewriter.end().unwrap();
        assert_eq!(*events.borrow(), [3, 1]);
    }
}

#[test]
fn callback_captures_release_on_eof_abort_and_error() {
    for outcome in ["eof", "abort", "error"] {
        let dropped = Rc::new(Cell::new(0));
        let owner = Rc::clone(&dropped);
        let settings =
            Settings::new().append_element_content_handler(element!("span", move |el| {
                let capture = Capture(Rc::clone(&owner));
                el.on_end_tag(end_tag!(move |_| {
                    let _keep_capture = &capture;
                    Err("deliberate callback failure".into())
                }))?;
                Ok(())
            }));
        let mut rewriter = HtmlRewriter::new(settings, |_: &[u8]| {});
        rewriter.write(b"<span>").unwrap();
        assert_eq!(dropped.get(), 0);
        match outcome {
            "eof" => rewriter.end().unwrap(),
            "abort" => drop(rewriter),
            _ => {
                assert!(rewriter.write(b"</span>").is_err());
                drop(rewriter);
            }
        }
        assert_eq!(dropped.get(), 1, "outcome={outcome}");
        assert_eq!(Rc::strong_count(&dropped), 1);
    }
}
