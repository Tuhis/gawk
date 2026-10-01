//! Stamps the build revision the window header shows (crates/ui/build_rev.rs
//! says why the shell does it, not gawk-ui).

#[path = "../ui/build_rev.rs"]
mod build_rev;

fn main() {
    build_rev::emit();
}
