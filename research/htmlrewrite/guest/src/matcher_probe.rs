use lol_html::{ElementContentHandlers, HtmlRewriter, MatcherEvent, MemorySettings, Settings};

pub(crate) fn settings() -> Settings<'static, 'static> {
    let mut settings = Settings::new()
        .with_memory_settings(MemorySettings::default().with_max_allowed_memory_usage(1 << 20))
        .with_matcher_observer(|event| {
            let line = match event {
                MatcherEvent::Match {
                    element,
                    rule,
                    content,
                } => {
                    format!("M {rule} {element} {}\n", u8::from(content))
                }
                MatcherEvent::Retire { element, explicit } => {
                    format!("R {element} {}\n", u8::from(explicit))
                }
            };
            super::output(line.as_bytes());
        });
    for (rule, selector) in ["*", "span[data-scope]"].into_iter().enumerate() {
        settings = settings.append_element_content_handler((
            std::borrow::Cow::Owned(selector.parse::<lol_html::Selector>().unwrap()),
            ElementContentHandlers::default()
                .element(move |el: &mut lol_html::html_content::Element<'_, '_>| {
                    super::output(format!("E {rule} {}\n", el.tag_name()).as_bytes());
                    Ok(())
                })
                .text(move |text: &mut lol_html::html_content::TextChunk<'_>| {
                    super::output(
                        format!(
                            "T {rule} {} {:?}\n",
                            u8::from(text.last_in_text_node()),
                            text.as_str()
                        )
                        .as_bytes(),
                    );
                    Ok(())
                })
                .comments(move |comment: &mut lol_html::html_content::Comment<'_>| {
                    super::output(format!("C {rule} {:?}\n", comment.text()).as_bytes());
                    Ok(())
                }),
        ));
    }
    settings
}

pub fn native_matcher_probe(input: &[u8], chunk: usize) -> Result<Vec<u8>, String> {
    super::OUTPUT.with_borrow_mut(Vec::clear);
    let mut rewriter = HtmlRewriter::new(settings(), |_: &[u8]| {});
    for bytes in input.chunks(chunk) {
        rewriter.write(bytes).map_err(|error| error.to_string())?;
    }
    rewriter.end().map_err(|error| error.to_string())?;
    Ok(super::OUTPUT.with_borrow_mut(std::mem::take))
}
