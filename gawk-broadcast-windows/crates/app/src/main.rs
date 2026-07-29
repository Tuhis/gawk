//! WB0 placeholder shell. The Slint GUI lands in WB6 (docs/38 D12); until
//! then the binary exists so packaging, CI artifact upload and the
//! release-please wiring have something real to carry.

fn main() {
    println!(
        "gawk-broadcast (Windows) {} — WB0 scaffold; GUI lands in WB6 (docs/38).",
        env!("CARGO_PKG_VERSION"),
    );
    println!("default relay:     {}", gawk_engine::defaults::RELAY_URL);
    println!("default app URL:   {}", gawk_engine::defaults::APP_URL);
    println!(
        "wire mirror:       {} message types pinned",
        gawk_wire::TYPE_STRIPE_STATE
    );
}
