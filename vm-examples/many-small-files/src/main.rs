use anyhow::Result;
use byteorder::{LittleEndian, WriteBytesExt};
use clap::Parser;
use log::info;
use rand::Rng;
use std::{
    fs::{self, OpenOptions},
    path::PathBuf,
};
use std::io::Write;

#[derive(Parser)]
#[command(author, version, about, long_about = None)]
struct Args {
    /// Number of files to create
    #[arg(short, long)]
    files: u32,

    /// Directory where to create files
    #[arg(short, long)]
    directory: PathBuf,
}

struct FileCreator;

impl FileCreator {
    fn create_many_files(&self, dir: &PathBuf, files: u32) -> Result<()> {
        // Create a directory with many files to simulate a large basebackup.
        fs::create_dir_all(dir)?;

        for i in 0..files {
            // format filename with leading zeros
            let filename = format!("{:0>7}", i);
            let dst = dir.join(&filename);

            if i % 100_000 == 0 {
                info!("Creating file {}", dst.display());
            }

            (|| -> Result<()> {
                let mut f = OpenOptions::new()
                    .write(true)
                    .create(true)
                    .truncate(true)
                    .open(dst)?;

                let mut data = Vec::<u8>::new();
                // write random u64
                for _ in 0..18 {
                    data.write_u64::<LittleEndian>(rand::thread_rng().gen::<u64>())?;
                }

                f.write_all(&data)?;
                Ok(())
            })()?;
        }

        info!("Finished creating {} files in {}", files, dir.display());

        Ok(())
    }
}

fn main() -> Result<()> {
    if std::env::var_os("RUST_LOG").is_none() {
        std::env::set_var("RUST_LOG", "info");
    }
    env_logger::init();
    
    let args = Args::parse();
    let creator = FileCreator;
    creator.create_many_files(&args.directory, args.files)?;
    
    Ok(())
}
