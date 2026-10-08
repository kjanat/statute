use lol_html::html_content::ContentType;
use lol_html::{HtmlRewriter, MemorySettings, Settings, element};
use std::cell::RefCell;

mod matcher_probe;
pub use matcher_probe::native_matcher_probe;

const INPUT_SIZE: usize = 65536;
#[cfg(target_arch = "wasm32")]
const OUTPUT_SIZE: usize = 16384;
static mut INPUT: [u8; INPUT_SIZE] = [0; INPUT_SIZE];
type Rewriter = HtmlRewriter<'static, fn(&[u8])>;

thread_local! {
    static REWRITER: RefCell<Option<Rewriter>> = const { RefCell::new(None) };
    static OUTPUT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
}

#[cfg(target_arch = "wasm32")]
struct Batch {
    bytes: [u8; OUTPUT_SIZE],
    length: usize,
    enabled: bool,
}

#[cfg(target_arch = "wasm32")]
impl Batch {
    fn flush(&mut self) {
        if self.length != 0 {
            unsafe { emit(self.bytes.as_ptr() as u32, self.length as u32) };
            self.length = 0;
        }
    }

    fn write(&mut self, mut bytes: &[u8]) {
        if !self.enabled {
            unsafe { emit(bytes.as_ptr() as u32, bytes.len() as u32) };
            return;
        }
        while !bytes.is_empty() {
            let count = bytes.len().min(OUTPUT_SIZE - self.length);
            self.bytes[self.length..self.length + count].copy_from_slice(&bytes[..count]);
            self.length += count;
            bytes = &bytes[count..];
            if self.length == OUTPUT_SIZE {
                self.flush();
            }
        }
    }
}

#[cfg(target_arch = "wasm32")]
thread_local! {
    static BATCH: RefCell<Batch> = const { RefCell::new(Batch {
        bytes: [0; OUTPUT_SIZE], length: 0, enabled: true,
    }) };
}

#[cfg(target_arch = "wasm32")]
#[link(wasm_import_module = "sink")]
unsafe extern "C" {
    fn emit(pointer: u32, length: u32);
}

fn output(bytes: &[u8]) {
    #[cfg(target_arch = "wasm32")]
    BATCH.with_borrow_mut(|batch| batch.write(bytes));
    #[cfg(not(target_arch = "wasm32"))]
    OUTPUT.with_borrow_mut(|out| out.extend_from_slice(bytes));
}

fn flush_output() {
    #[cfg(target_arch = "wasm32")]
    BATCH.with_borrow_mut(Batch::flush);
}

fn settings() -> Settings<'static, 'static> {
    Settings::new()
        .with_memory_settings(MemorySettings::default().with_max_allowed_memory_usage(1 << 20))
        .append_element_content_handler(element!("a.rewrite", |el| {
            el.set_attribute("href", "https://example.invalid/rewritten")?;
            el.append("<em>inserted</em>", ContentType::Html);
            Ok(())
        }))
        .append_element_content_handler(element!(".remove", |el| {
            el.remove();
            Ok(())
        }))
}

// One mutable parser per instance. The host destroys the instance after any
// error; it never pools a partially consumed or poisoned parser.
#[cfg_attr(target_arch = "wasm32", unsafe(no_mangle))]
pub extern "C" fn create(buffered: u32) -> u32 {
    create_with(buffered, settings(), output as fn(&[u8]))
}

// Private parity probe; it emits matcher/content events instead of HTML.
#[cfg_attr(target_arch = "wasm32", unsafe(no_mangle))]
pub extern "C" fn create_matcher_probe(buffered: u32) -> u32 {
    create_with(buffered, matcher_probe::settings(), |_| {})
}

fn create_with(buffered: u32, settings: Settings<'static, 'static>, sink: fn(&[u8])) -> u32 {
    if buffered > 1 {
        return 5;
    }
    REWRITER.with_borrow_mut(|slot| {
        if slot.is_some() {
            return 1;
        }
        #[cfg(target_arch = "wasm32")]
        BATCH.with_borrow_mut(|batch| {
            batch.enabled = buffered == 1;
            batch.length = 0;
        });
        *slot = Some(HtmlRewriter::new(settings, sink));
        0
    })
}

#[cfg_attr(target_arch = "wasm32", unsafe(no_mangle))]
pub extern "C" fn input_pointer() -> *mut u8 {
    std::ptr::addr_of_mut!(INPUT).cast::<u8>()
}

#[cfg_attr(target_arch = "wasm32", unsafe(no_mangle))]
pub extern "C" fn write(length: u32) -> u32 {
    if length as usize > INPUT_SIZE {
        return 2;
    }
    // Calls are serialized by the Go owner; the fixed input region avoids an
    // allocator ABI and never escapes an invocation of write.
    let input = unsafe {
        std::slice::from_raw_parts(std::ptr::addr_of!(INPUT).cast::<u8>(), length as usize)
    };
    REWRITER.with_borrow_mut(|slot| {
        let ok = slot
            .as_mut()
            .is_some_and(|rewriter| rewriter.write(input).is_ok());
        if ok {
            flush_output();
            0
        } else {
            *slot = None;
            3
        }
    })
}

#[cfg_attr(target_arch = "wasm32", unsafe(no_mangle))]
pub extern "C" fn finish() -> u32 {
    REWRITER.with_borrow_mut(|slot| match slot.take() {
        Some(rewriter) => {
            if rewriter.end().is_ok() {
                flush_output();
                0
            } else {
                4
            }
        }
        None => 4,
    })
}

// The native executable uses the identical policy and crate revision as the
// Wasm guest, providing an output oracle without a second rewrite algorithm.
pub fn native(input: &[u8], chunk: usize) -> Result<Vec<u8>, String> {
    OUTPUT.with_borrow_mut(Vec::clear);
    let mut rewriter = HtmlRewriter::new(settings(), output as fn(&[u8]));
    for bytes in input.chunks(chunk) {
        rewriter.write(bytes).map_err(|e| e.to_string())?;
    }
    rewriter.end().map_err(|e| e.to_string())?;
    Ok(OUTPUT.with_borrow_mut(std::mem::take))
}
