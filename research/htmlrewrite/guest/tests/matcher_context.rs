use lol_html::{
    ElementContentHandlers, HtmlRewriter, MatcherEvent, MemorySettings, Settings, element,
};
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

#[derive(Default, Debug)]
struct Observation {
    events: Vec<MatcherEvent>,
    scopes: BTreeMap<u64, Vec<u32>>,
    text: Vec<(u32, String, Vec<u64>)>,
    comments: Vec<(u32, String, Vec<u64>)>,
}

fn observe(state: &Arc<Mutex<Observation>>, event: MatcherEvent) {
    let mut state = state.lock().unwrap();
    state.events.push(event);
    match event {
        MatcherEvent::Match {
            element,
            rule,
            content: true,
        } => {
            state.scopes.entry(element).or_default().push(rule);
        }
        MatcherEvent::Retire { element, .. } => {
            assert!(state.scopes.remove(&element).is_some());
        }
        _ => {}
    }
}

fn settings(state: &Arc<Mutex<Observation>>) -> Settings<'static, 'static> {
    let observer = Arc::clone(state);
    let mut settings =
        Settings::new().with_matcher_observer(move |event| observe(&observer, event));
    for (rule, selector) in ["span", "span[data-scope]"].into_iter().enumerate() {
        let text_state = Arc::clone(state);
        let comment_state = Arc::clone(state);
        settings = settings.append_element_content_handler((
            std::borrow::Cow::Owned(selector.parse::<lol_html::Selector>().unwrap()),
            ElementContentHandlers::default()
                .text(move |text: &mut lol_html::html_content::TextChunk<'_>| {
                    let mut state = text_state.lock().unwrap();
                    let scopes = state
                        .scopes
                        .iter()
                        .filter_map(|(&id, rules)| rules.contains(&(rule as u32)).then_some(id))
                        .collect();
                    if !text.as_str().is_empty() {
                        state.text.push((rule as u32, text.as_str().into(), scopes));
                    }
                    Ok(())
                })
                .comments(move |comment: &mut lol_html::html_content::Comment<'_>| {
                    let mut state = comment_state.lock().unwrap();
                    let scopes = state
                        .scopes
                        .iter()
                        .filter_map(|(&id, rules)| rules.contains(&(rule as u32)).then_some(id))
                        .collect();
                    state.comments.push((rule as u32, comment.text(), scopes));
                    Ok(())
                }),
        ));
    }
    settings
}

#[test]
fn text_only_rules_observe_nested_scopes_and_implicit_retirement() {
    for chunk in [1, 7, 4096] {
        let state = Arc::new(Mutex::new(Observation::default()));
        let mut rewriter = HtmlRewriter::new(settings(&state), |_: &[u8]| {});
        let input =
            b"<div><span data-scope><span data-scope>x<!--in--></div>y<!--out--><span>z</span>";
        for bytes in input.chunks(chunk) {
            rewriter.write(bytes).unwrap();
        }
        rewriter.end().unwrap();
        let state = state.lock().unwrap();
        assert_eq!(
            state.text,
            [
                (0, "x".into(), vec![2, 3]),
                (1, "x".into(), vec![2, 3]),
                (0, "z".into(), vec![4])
            ]
        );
        assert_eq!(
            state.comments,
            [(0, "in".into(), vec![2, 3]), (1, "in".into(), vec![2, 3])]
        );
        assert!(state.scopes.is_empty());
        let retired: Vec<_> = state
            .events
            .iter()
            .filter_map(|event| match event {
                MatcherEvent::Retire { element, explicit } => Some((*element, *explicit)),
                _ => None,
            })
            .collect();
        assert_eq!(retired, [(2, false), (3, false), (4, true)]);
    }
}

#[test]
fn void_and_foreign_self_closing_matches_never_replace_parent_identity() {
    for input in ["<div><br><img src=x>x</div>", "<svg><path/><g>x</g></svg>"] {
        for chunk in [1, 7, 4096] {
            let state = Arc::new(Mutex::new(Observation::default()));
            let observer = Arc::clone(&state);
            let settings = Settings::new()
                .with_matcher_observer(move |event| observe(&observer, event))
                .append_element_content_handler(element!("*", |el| {
                    el.set_tag_name("renamed")?;
                    Ok(())
                }));
            let mut rewriter = HtmlRewriter::new(settings, |_: &[u8]| {});
            for bytes in input.as_bytes().chunks(chunk) {
                rewriter.write(bytes).unwrap();
            }
            rewriter.end().unwrap();
            let state = state.lock().unwrap();
            assert!(state.scopes.is_empty(), "{input}: {state:?}");
            assert_eq!(
                state.events.last(),
                Some(&MatcherEvent::Retire {
                    element: 1,
                    explicit: true
                })
            );
            assert!(!state.events.contains(&MatcherEvent::Retire {
                element: 2,
                explicit: true
            }));
        }
    }
}

#[test]
fn observers_are_parser_local_and_eof_does_not_invent_retirement() {
    let first = Arc::new(Mutex::new(Observation::default()));
    let second = Arc::new(Mutex::new(Observation::default()));
    let mut a = HtmlRewriter::new(settings(&first), |_: &[u8]| {});
    let mut b = HtmlRewriter::new(settings(&second), |_: &[u8]| {});
    a.write(b"<span data-scope>").unwrap();
    b.write(b"<div><span>").unwrap();
    a.write(b"a</span>").unwrap();
    b.write(b"b").unwrap();
    a.end().unwrap();
    b.end().unwrap();
    assert!(first.lock().unwrap().scopes.is_empty());
    assert_eq!(
        second
            .lock()
            .unwrap()
            .scopes
            .keys()
            .copied()
            .collect::<Vec<_>>(),
        [2]
    );
    assert_eq!(Arc::strong_count(&first), 1);
    assert_eq!(Arc::strong_count(&second), 1);
}

#[test]
fn internal_charset_selector_does_not_shift_registered_rule_ids() {
    let state = Arc::new(Mutex::new(Observation::default()));
    let observer = Arc::clone(&state);
    let settings = Settings::new()
        .with_adjust_charset_on_meta_tag(true)
        .with_matcher_observer(move |event| observe(&observer, event))
        .append_element_content_handler(element!("span", |_| Ok(())));
    let mut rewriter = HtmlRewriter::new(settings, |_: &[u8]| {});
    rewriter
        .write(b"<meta charset=ascii><span></span>")
        .unwrap();
    rewriter.end().unwrap();
    assert_eq!(
        state.lock().unwrap().events,
        [
            MatcherEvent::Match {
                element: 2,
                rule: 0,
                content: true
            },
            MatcherEvent::Retire {
                element: 2,
                explicit: true
            },
        ]
    );
}

#[test]
fn observer_capture_drops_after_abort_handler_failure_and_memory_failure() {
    for failure in ["abort", "handler", "memory"] {
        let state = Arc::new(Mutex::new(Observation::default()));
        let observer = Arc::clone(&state);
        let settings = Settings::new()
            .with_memory_settings(MemorySettings::default().with_max_allowed_memory_usage(4096))
            .with_matcher_observer(move |event| observe(&observer, event))
            .append_element_content_handler(element!("span", move |_| {
                if failure == "handler" {
                    return Err("deliberate failure".into());
                }
                Ok(())
            }));
        let mut rewriter = HtmlRewriter::new(settings, |_: &[u8]| {});
        if failure == "memory" {
            assert!(rewriter.write("<span>".repeat(4096).as_bytes()).is_err());
        } else {
            assert_eq!(rewriter.write(b"<span>").is_err(), failure == "handler");
        }
        drop(rewriter);
        assert_eq!(Arc::strong_count(&state), 1);
        assert!(
            !state
                .lock()
                .unwrap()
                .events
                .iter()
                .any(|event| matches!(event, MatcherEvent::Retire { .. }))
        );
    }
}
