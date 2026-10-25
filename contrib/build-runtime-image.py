"""Build and verify a local runtime image from a clean, committed checkout."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess


def run(args, *, cwd, timeout=60, env=None):
    result = subprocess.run(args, cwd=cwd, env=env, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError({"command": args, "returncode": result.returncode,
                            "stdout": result.stdout, "stderr": result.stderr})
    return result.stdout.strip()


def sha(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True,
                        help="Existing empty directory outside the source checkout")
    parser.add_argument("--go-version", default="1.25.14")
    args = parser.parse_args()
    source = Path(__file__).resolve().parent.parent
    output = args.output_dir.resolve()
    if not output.is_dir() or any(output.iterdir()) or output.is_relative_to(source):
        parser.error("output-dir must be an existing empty directory outside the checkout")
    status = run(["git", "status", "--porcelain"], cwd=source)
    if status:
        parser.error("commit source changes before building: " + status)
    revision = run(["git", "rev-parse", "HEAD"], cwd=source)
    image = "geesefs:memory-" + revision[:12]
    environment = os.environ | {"GOTOOLCHAIN": "go" + args.go_version,
                                "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64"}
    toolchain = run(["go", "version"], cwd=source, env=environment, timeout=120)
    binary = output / "geesefs"
    run(["go", "build", "-trimpath", "-buildvcs=true", "-ldflags",
         "-X github.com/yandex-cloud/geesefs/core/cfg.VersionHash=" + revision,
         "-o", str(binary), "."], cwd=source, env=environment, timeout=360)
    build_info = run(["go", "version", "-m", str(binary)], cwd=source, env=environment)
    if "vcs.revision=" + revision not in build_info or "vcs.modified=false" not in build_info:
        raise RuntimeError("binary does not identify the clean source revision")
    run(["docker", "build", "--network=none", "--build-arg", "VCS_REF=" + revision,
         "--file", str(source / "contrib/Dockerfile.runtime"), "--tag", image, str(output)],
        cwd=source, timeout=180)
    image_info = json.loads(run(["docker", "image", "inspect", image], cwd=source))[0]
    version = run(["docker", "run", "--rm", "--network=none", image, "--version"], cwd=source)
    container = run(["docker", "create", image], cwd=source)
    extracted = output / "image-geesefs"
    try:
        run(["docker", "cp", container + ":/geesefs", str(extracted)], cwd=source)
    finally:
        run(["docker", "rm", container], cwd=source)
    digest = sha(binary)
    if sha(extracted) != digest:
        raise RuntimeError("image binary differs from the checked build")
    archive = output / "image.tar"
    run(["docker", "save", "--output", str(archive), image], cwd=source, timeout=180)
    result = {"source_revision": revision, "go_version": toolchain, "image": image,
              "image_id": image_info["Id"], "image_version_output": version,
              "binary_sha256": digest, "binary_bytes": binary.stat().st_size,
              "archive_sha256": sha(archive), "archive_bytes": archive.stat().st_size,
              "build_info": build_info}
    with (output / "build.json").open("x") as stream:
        json.dump(result, stream, indent=2)
        stream.write("\n")
    print(json.dumps(result, indent=2), flush=True)


if __name__ == "__main__":
    main()
