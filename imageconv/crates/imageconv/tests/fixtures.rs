//! Conversion fixtures: layers converted by the real `mkfs.erofs` at the pinned version, and the
//! tree each leaves read back with the real `fsck.erofs`, against what the OCI image
//! specification says the layers make. The tools are the ones `make test` builds.

#[path = "support/image.rs"]
mod image;

use std::collections::VecDeque;
use std::fs;
use std::io::{self, Cursor, Read};
use std::path::Path;

use image::{Entry, GZIP, Result, TAR, Tree, UUID, ZSTD, expect, gzip, layer, zstd};
use imageconv::{Error, Layers, Mkfs, MkfsError, Parameters, Pass, PassKind, Step, convert};
use tempfile::TempDir;

/// Layers streamed from memory in the order the contract requires: for each layer a names pass
/// and then a content pass, each carrying the layer's bytes.
struct Streamed {
    passes: VecDeque<(Pass, Vec<u8>)>,
    current: Cursor<Vec<u8>>,
}

impl Streamed {
    fn new(layers: &[&[u8]]) -> Self {
        let mut passes = Vec::new();
        for (layer, bytes) in (0u32..).zip(layers) {
            for kind in [PassKind::Names, PassKind::Content] {
                passes.push((Pass { layer, kind }, bytes.to_vec()));
            }
        }
        Self::in_order(passes)
    }

    fn in_order(passes: Vec<(Pass, Vec<u8>)>) -> Self {
        Self {
            passes: passes.into(),
            current: Cursor::default(),
        }
    }
}

impl Layers for Streamed {
    fn next_pass(&mut self) -> io::Result<Option<Pass>> {
        Ok(self.passes.pop_front().map(|(pass, bytes)| {
            self.current = Cursor::new(bytes);
            pass
        }))
    }

    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        self.current.read(buf)
    }
}

/// An image of layers, each `(media type, bytes)`.
type Image = [(&'static str, Vec<u8>)];

fn media_types(image: &Image) -> Vec<String> {
    image.iter().map(|(media, _)| (*media).to_owned()).collect()
}

fn streamed(image: &Image) -> Streamed {
    let bytes: Vec<&[u8]> = image.iter().map(|(_, bytes)| bytes.as_slice()).collect();
    Streamed::new(&bytes)
}

/// Convert `image` with the pinned `mkfs.erofs` and return the tree of the result. The length the
/// converter reports is the file's: `mkfs.erofs` truncates a regular output to its image.
fn convert_to_tree(image: &Image) -> Result<Tree> {
    let dir = TempDir::new()?;
    let output = image::output_file(dir.path())?;
    let parameters = Parameters::new(UUID, &media_types(image), 1 << 20)?;
    let length = convert(
        &parameters,
        &mut streamed(image),
        &Mkfs::new(image::tool("MKFS_EROFS")),
        &output,
    )?;
    assert_eq!(length, fs::metadata(&output)?.len());
    assert_eq!(length % 4096, 0, "a whole number of blocks");
    image::tree(&output, dir.path())
}

fn plain(layers: Vec<Vec<u8>>) -> Vec<(&'static str, Vec<u8>)> {
    layers.into_iter().map(|bytes| (TAR, bytes)).collect()
}

#[test]
fn a_whiteout_hides_what_lower_layers_hold_and_is_itself_absent() -> Result {
    let tree = convert_to_tree(&plain(vec![
        layer(&[
            Entry::Dir("d"),
            Entry::File("d/a", "A"),
            Entry::File("d/b", "B"),
            Entry::File("x", "X"),
            Entry::File("keep", "K"),
        ]),
        layer(&[Entry::Whiteout("x"), Entry::Whiteout("d/a")]),
    ]))?;

    assert_eq!(tree, expect(&["d/", "d/b=B", "keep=K"]));
    Ok(())
}

#[test]
fn an_opaque_whiteout_hides_every_lower_child_of_its_directory_and_not_the_layers_own() -> Result {
    let tree = convert_to_tree(&plain(vec![
        layer(&[
            Entry::Dir("d"),
            Entry::File("d/a", "A"),
            Entry::File("d/b", "B"),
            Entry::Dir("e"),
            Entry::File("e/f", "F"),
        ]),
        layer(&[Entry::Dir("d"), Entry::Opaque("d"), Entry::File("d/c", "C")]),
    ]))?;

    assert_eq!(tree, expect(&["d/", "d/c=C", "e/", "e/f=F"]));
    Ok(())
}

#[test]
fn a_directory_a_later_layer_lists_without_the_opaque_marker_keeps_its_lower_children() -> Result {
    let tree = convert_to_tree(&plain(vec![
        layer(&[Entry::Dir("d"), Entry::File("d/a", "A")]),
        layer(&[Entry::Dir("d"), Entry::File("d/b", "B")]),
    ]))?;

    assert_eq!(tree, expect(&["d/", "d/a=A", "d/b=B"]));
    Ok(())
}

#[test]
fn a_directory_replaced_by_a_file_loses_its_children() -> Result {
    let tree = convert_to_tree(&plain(vec![
        layer(&[
            Entry::Dir("d"),
            Entry::File("d/a", "A"),
            Entry::File("d/b", "B"),
            Entry::File("keep", "K"),
        ]),
        layer(&[Entry::File("d", "now a file")]),
    ]))?;

    assert_eq!(tree, expect(&["d=now a file", "keep=K"]));
    Ok(())
}

#[test]
fn a_file_replaced_by_a_directory_takes_the_directory() -> Result {
    let tree = convert_to_tree(&plain(vec![
        layer(&[Entry::File("f", "was a file"), Entry::File("keep", "K")]),
        layer(&[Entry::Dir("f"), Entry::File("f/g", "G")]),
    ]))?;

    assert_eq!(tree, expect(&["f/", "f/g=G", "keep=K"]));
    Ok(())
}

/// The lower layer for the same-layer fixtures: a file and a directory with a child.
fn lower() -> Vec<u8> {
    layer(&[
        Entry::File("f", "old"),
        Entry::Dir("d"),
        Entry::File("d/old", "O"),
    ])
}

#[test]
fn a_whiteout_before_a_file_of_its_name_in_one_layer_leaves_the_layers_file() -> Result {
    let tree = convert_to_tree(&plain(vec![
        lower(),
        layer(&[Entry::Whiteout("f"), Entry::File("f", "new")]),
    ]))?;

    assert_eq!(tree, expect(&["f=new", "d/", "d/old=O"]));
    Ok(())
}

#[test]
fn a_whiteout_after_a_file_of_its_name_in_one_layer_leaves_the_layers_file() -> Result {
    let tree = convert_to_tree(&plain(vec![
        lower(),
        layer(&[Entry::File("f", "new"), Entry::Whiteout("f")]),
    ]))?;

    assert_eq!(tree, expect(&["f=new", "d/", "d/old=O"]));
    Ok(())
}

#[test]
fn a_whiteout_before_a_directory_of_its_name_in_one_layer_leaves_only_the_layers_children() -> Result
{
    let tree = convert_to_tree(&plain(vec![
        lower(),
        layer(&[
            Entry::Whiteout("d"),
            Entry::Dir("d"),
            Entry::File("d/new", "N"),
        ]),
    ]))?;

    assert_eq!(tree, expect(&["f=old", "d/", "d/new=N"]));
    Ok(())
}

#[test]
fn a_whiteout_after_a_directory_of_its_name_in_one_layer_leaves_only_the_layers_children() -> Result
{
    let tree = convert_to_tree(&plain(vec![
        lower(),
        layer(&[
            Entry::Dir("d"),
            Entry::File("d/new", "N"),
            Entry::Whiteout("d"),
        ]),
    ]))?;

    assert_eq!(tree, expect(&["f=old", "d/", "d/new=N"]));
    Ok(())
}

#[test]
fn a_whiteout_of_a_name_the_layer_holds_only_children_of_loses_the_lower_ones() -> Result {
    let tree = convert_to_tree(&plain(vec![
        lower(),
        layer(&[Entry::File("d/new", "N"), Entry::Whiteout("d")]),
    ]))?;

    assert_eq!(tree, expect(&["f=old", "d/", "d/new=N"]));
    Ok(())
}

#[test]
fn gzip_layers_are_converted_with_and_without_a_rewrite() -> Result {
    let tree = convert_to_tree(&[
        (
            GZIP,
            gzip(&layer(&[
                Entry::Dir("d"),
                Entry::File("d/a", "A"),
                Entry::File("x", "X"),
                Entry::File("f", "old"),
            ])),
        ),
        // Nothing to rewrite: the tool filters the layer itself.
        (
            GZIP,
            gzip(&layer(&[Entry::Whiteout("x"), Entry::File("d/c", "C")])),
        ),
        // A same-layer whiteout: the converter decompresses and rewrites the layer.
        (
            GZIP,
            gzip(&layer(&[Entry::Whiteout("f"), Entry::File("f", "new")])),
        ),
    ])?;

    assert_eq!(tree, expect(&["d/", "d/a=A", "d/c=C", "f=new"]));
    Ok(())
}

#[test]
fn zstd_layers_are_converted_with_and_without_a_rewrite() -> Result {
    let tree = convert_to_tree(&[
        (
            ZSTD,
            zstd(&layer(&[
                Entry::Dir("d"),
                Entry::File("d/a", "A"),
                Entry::File("x", "X"),
                Entry::File("f", "old"),
            ])),
        ),
        (
            ZSTD,
            zstd(&layer(&[Entry::Whiteout("x"), Entry::File("d/c", "C")])),
        ),
        (
            ZSTD,
            zstd(&layer(&[Entry::File("f", "new"), Entry::Whiteout("f")])),
        ),
    ])?;

    assert_eq!(tree, expect(&["d/", "d/a=A", "d/c=C", "f=new"]));
    Ok(())
}

#[test]
fn layers_of_different_media_types_make_one_image() -> Result {
    let tree = convert_to_tree(&[
        (TAR, layer(&[Entry::File("a", "A"), Entry::File("b", "B")])),
        (
            GZIP,
            gzip(&layer(&[Entry::Whiteout("a"), Entry::File("c", "C")])),
        ),
        (
            ZSTD,
            zstd(&layer(&[Entry::Whiteout("b"), Entry::File("d", "D")])),
        ),
    ])?;

    assert_eq!(tree, expect(&["c=C", "d=D"]));
    Ok(())
}

#[test]
fn a_layer_media_type_the_converter_does_not_take_is_refused_before_any_layer_is_read() {
    let docker = "application/vnd.docker.image.rootfs.diff.tar.gzip".to_owned();
    let accepted = [TAR.to_owned(), GZIP.to_owned()];
    let refused = [TAR.to_owned(), docker.clone()];

    // The control: the same image with every type accepted is accepted.
    assert!(Parameters::new(UUID, &accepted, 1 << 20).is_ok());
    let error = Parameters::new(UUID, &refused, 1 << 20).unwrap_err();
    assert!(
        matches!(&error, Error::LayerType { layer: 1, media_type } if *media_type == docker),
        "{error}"
    );
    assert_eq!(error.step(), Step::LayerType);
}

#[test]
fn a_start_without_layers_or_with_a_bad_uuid_is_unusable() {
    let accepted = [TAR.to_owned()];
    assert!(Parameters::new(UUID, &accepted, 1).is_ok());
    for error in [
        Parameters::new(UUID, &[], 1).unwrap_err(),
        Parameters::new("not a uuid", &accepted, 1).unwrap_err(),
        Parameters::new("--tar=f", &accepted, 1).unwrap_err(),
    ] {
        assert_eq!(error.step(), Step::Start, "{error}");
    }
}

#[test]
fn a_layer_that_is_not_of_its_media_type_fails_its_names_pass() -> Result {
    let dir = TempDir::new()?;
    let output = image::output_file(dir.path())?;
    let image = [(GZIP, b"this is not gzip".to_vec())];
    let parameters = Parameters::new(UUID, &media_types(&image), 1 << 20)?;

    let error = convert(
        &parameters,
        &mut streamed(&image),
        &Mkfs::new(image::tool("MKFS_EROFS")),
        &output,
    )
    .unwrap_err();

    assert_eq!(error.step(), Step::Names, "{error}");
    Ok(())
}

#[test]
fn entry_names_past_the_limit_fail_the_names_pass_with_the_limit() -> Result {
    let dir = TempDir::new()?;
    let output = image::output_file(dir.path())?;
    let image = plain(vec![layer(&[
        Entry::File("a-long-name", "1"),
        Entry::File("another-long-name", "2"),
    ])]);
    let mkfs = Mkfs::new(image::tool("MKFS_EROFS"));

    // 11 + 17 bytes of names: the exact limit is accepted, one byte less is not.
    let accepted = Parameters::new(UUID, &media_types(&image), 28)?;
    assert!(convert(&accepted, &mut streamed(&image), &mkfs, &output).is_ok());
    let refused = Parameters::new(UUID, &media_types(&image), 27)?;
    let error = convert(&refused, &mut streamed(&image), &mkfs, &output).unwrap_err();

    assert!(
        matches!(
            error,
            Error::NameLimit {
                layer: 0,
                limit: 27
            }
        ),
        "{error}"
    );
    Ok(())
}

#[test]
fn passes_out_of_the_contracts_order_are_refused() -> Result {
    let dir = TempDir::new()?;
    let output = image::output_file(dir.path())?;
    let bytes = layer(&[Entry::File("a", "A")]);
    let names = Pass {
        layer: 0,
        kind: PassKind::Names,
    };
    let content = Pass {
        layer: 0,
        kind: PassKind::Content,
    };
    let parameters = Parameters::new(UUID, &[TAR.to_owned()], 1 << 20)?;
    let mkfs = Mkfs::new(image::tool("MKFS_EROFS"));
    let run = |passes: Vec<(Pass, Vec<u8>)>| {
        convert(&parameters, &mut Streamed::in_order(passes), &mkfs, &output)
    };

    // The control: the order the contract requires converts.
    assert!(run(vec![(names, bytes.clone()), (content, bytes.clone())]).is_ok());
    for passes in [
        vec![(content, bytes.clone()), (names, bytes.clone())],
        vec![(names, bytes.clone())],
        vec![],
        vec![
            (names, bytes.clone()),
            (content, bytes.clone()),
            (names, bytes.clone()),
        ],
        vec![(
            Pass {
                layer: 1,
                kind: PassKind::Names,
            },
            bytes.clone(),
        )],
    ] {
        let error = run(passes).unwrap_err();
        assert_eq!(error.step(), Step::Stream, "{error}");
    }
    Ok(())
}

/// A stand-in for `mkfs.erofs` that logs its arguments to `log`, reads its input and exits with
/// `code`. The script is run as an argument of `/bin/sh` and never executed itself: a script
/// written and then executed fails with ETXTBSY when another test thread forks while it is open
/// for writing, since the child holds the descriptor until its own exec.
fn fake_mkfs(dir: &Path, log: &Path, code: i32) -> Result<Mkfs> {
    let script = dir.join("mkfs.sh");
    fs::write(
        &script,
        format!(
            "echo \"$@\" >> {}\ncat > /dev/null\necho 'it broke' >&2\nexit {code}\n",
            log.display()
        ),
    )?;
    Ok(Mkfs::with_leading_args("/bin/sh", [script]))
}

#[test]
fn a_fake_tool_runs_while_another_process_holds_its_script_open_for_writing() -> Result {
    use std::os::unix::fs::OpenOptionsExt;

    let dir = TempDir::new()?;
    let log = dir.path().join("log");
    let output = image::output_file(dir.path())?;
    let image = plain(vec![layer(&[Entry::File("a", "A")])]);
    let parameters = Parameters::new(UUID, &media_types(&image), 1 << 20)?;
    let mkfs = fake_mkfs(dir.path(), &log, 0)?;
    // What a forked child of another test thread holds: a write descriptor on the script.
    let held = fs::OpenOptions::new()
        .write(true)
        .open(dir.path().join("mkfs.sh"))?;

    let error = convert(&parameters, &mut streamed(&image), &mkfs, &output).unwrap_err();

    // The fake ran: the conversion got as far as the output, which the fake never wrote.
    assert_eq!(error.step(), Step::Output, "{error}");
    assert_eq!(fs::read_to_string(&log)?.lines().count(), 1);
    // Control: the same script executed itself is refused as busy, which is the race.
    let direct = dir.path().join("direct.sh");
    drop(
        fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o755)
            .open(&direct)?,
    );
    let held_direct = fs::OpenOptions::new().write(true).open(&direct)?;
    let busy = std::process::Command::new(&direct).spawn().unwrap_err();
    assert_eq!(busy.kind(), io::ErrorKind::ExecutableFileBusy, "{busy}");
    drop((held, held_direct));
    Ok(())
}

#[test]
fn mkfs_runs_once_per_layer_full_then_incremental_and_filters_gzip_only_where_the_layer_came_whole()
-> Result {
    let dir = TempDir::new()?;
    let log = dir.path().join("log");
    let output = image::output_file(dir.path())?;
    let image = [
        (TAR, layer(&[Entry::File("a", "A")])),
        (GZIP, gzip(&layer(&[Entry::File("b", "B")]))),
        (
            GZIP,
            gzip(&layer(&[Entry::Whiteout("c"), Entry::File("c", "C")])),
        ),
        (ZSTD, zstd(&layer(&[Entry::File("d", "D")]))),
    ];
    let parameters = Parameters::new(UUID, &media_types(&image), 1 << 20)?;
    let mkfs = fake_mkfs(dir.path(), &log, 0)?;

    // The fake writes no image, so the conversion ends at the output; the runs are already done.
    let error = convert(&parameters, &mut streamed(&image), &mkfs, &output).unwrap_err();
    assert_eq!(error.step(), Step::Output, "{error}");

    let runs: Vec<String> = fs::read_to_string(&log)?
        .lines()
        .map(str::to_owned)
        .collect();
    let common = format!("--quiet -T0 --mkfs-time --tar=f --aufs --ovlfs-strip=1 -U{UUID}");
    let out = output.display();
    assert_eq!(
        runs,
        [
            format!("{common} {out}"),
            format!("{common} --incremental=data --ungzip {out}"),
            format!("{common} --incremental=data {out}"),
            format!("{common} --incremental=data {out}"),
        ]
    );
    Ok(())
}

#[test]
fn a_tool_that_fails_is_a_convert_failure_carrying_what_it_wrote() -> Result {
    let dir = TempDir::new()?;
    let log = dir.path().join("log");
    let output = image::output_file(dir.path())?;
    let image = plain(vec![
        layer(&[Entry::File("a", "A")]),
        layer(&[Entry::File("b", "B")]),
    ]);
    let parameters = Parameters::new(UUID, &media_types(&image), 1 << 20)?;

    let error = convert(
        &parameters,
        &mut streamed(&image),
        &fake_mkfs(dir.path(), &log, 3)?,
        &output,
    )
    .unwrap_err();

    assert_eq!(error.step(), Step::Convert);
    let Error::Convert {
        layer: 0,
        source: MkfsError::Failed { status, stderr },
    } = &error
    else {
        panic!("{error}");
    };
    assert!(status.contains('3'), "{status}");
    assert_eq!(stderr, "it broke");
    // The second layer was never run.
    assert_eq!(fs::read_to_string(&log)?.lines().count(), 1);
    Ok(())
}

#[test]
fn a_tool_that_is_missing_is_a_convert_failure() -> Result {
    let dir = TempDir::new()?;
    let output = image::output_file(dir.path())?;
    let image = plain(vec![layer(&[Entry::File("a", "A")])]);
    let parameters = Parameters::new(UUID, &media_types(&image), 1 << 20)?;

    let error = convert(
        &parameters,
        &mut streamed(&image),
        &Mkfs::new(dir.path().join("absent")),
        &output,
    )
    .unwrap_err();

    assert!(
        matches!(
            error,
            Error::Convert {
                source: MkfsError::Start { .. },
                ..
            }
        ),
        "{error}"
    );
    Ok(())
}

#[test]
fn the_image_carries_the_uuid_that_was_asked_for_and_the_same_layers_give_the_same_image() -> Result
{
    let dir = TempDir::new()?;
    let image = [
        (TAR, layer(&[Entry::Dir("d"), Entry::File("d/a", "A")])),
        (GZIP, gzip(&layer(&[Entry::File("b", "B")]))),
    ];
    let mkfs = Mkfs::new(image::tool("MKFS_EROFS"));
    let mut images = Vec::new();
    for uuid in [UUID, UUID, "99999999-2222-3333-4444-555555555555"] {
        let output = image::output_file(dir.path())?;
        let parameters = Parameters::new(uuid, &media_types(&image), 1 << 20)?;
        convert(&parameters, &mut streamed(&image), &mkfs, &output)?;
        images.push(fs::read(&output)?);
    }

    // The UUID is at offset 48 of the superblock, which starts at 1024.
    assert_eq!(&images[0][1024 + 48..1024 + 52], [0x11; 4]);
    assert_eq!(&images[2][1024 + 48..1024 + 52], [0x99; 4]);
    assert_eq!(images[0], images[1]);
    assert_ne!(images[0], images[2]);
    Ok(())
}
