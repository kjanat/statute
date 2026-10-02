use std::hint::black_box;
use std::time::Instant;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let iterations: u32 = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "1000".to_string())
        .parse()?;
    if iterations == 0 {
        return Err("iterations must be positive".into());
    }
    for shape in ["dense", "sparse"] {
        for count in [16, 1024] {
            let mut input = String::new();
            for i in 0..count {
                input.push_str(if shape == "dense" || i % 32 == 0 {
                    r#"<a class="rewrite" href="old">payload</a>"#
                } else {
                    "<p>plain text without a matching class</p>"
                });
            }
            for chunk in [1, 4096] {
                for _ in 0..10 {
                    black_box(statute_htmlrewrite_spike::native(input.as_bytes(), chunk)?);
                }
                for _ in 0..3 {
                    let start = Instant::now();
                    for _ in 0..iterations {
                        black_box(statute_htmlrewrite_spike::native(
                            black_box(input.as_bytes()),
                            chunk,
                        )?);
                    }
                    let elapsed = start.elapsed();
                    println!(
                        "BenchmarkNative/shape={shape}/bytes={}/chunk={chunk} {iterations} {:.0} ns/op {:.2} MB/s",
                        input.len(),
                        elapsed.as_nanos() as f64 / f64::from(iterations),
                        input.len() as f64 * f64::from(iterations) / elapsed.as_secs_f64() / 1e6,
                    );
                }
            }
        }
    }
    Ok(())
}
