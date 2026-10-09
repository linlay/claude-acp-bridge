"""Package only declared release files, never local config or stale build files."""
import argparse
import json
from pathlib import Path
import stat
import zipfile

parser = argparse.ArgumentParser()
parser.add_argument("--version", required=True)
parser.add_argument("--arch", choices=["arm64", "amd64"], required=True)
parser.add_argument("--output", required=True)
args = parser.parse_args()
root = Path(__file__).resolve().parent.parent
manifest = json.loads((root / "manifest.json").read_text())
plugin_id = manifest["id"]
manifest["version"] = args.version
manifest["platform"] = {"os": "darwin", "arch": args.arch}
files = {name: root / name for name in [".env.example", "start.sh", "stop.sh", "deploy.sh", "scripts/program-common.sh"]}
files[f"backend/{plugin_id}"] = root / "dist" / "bundle" / plugin_id / "backend" / plugin_id
assert set(manifest["runtime"]["requiredPaths"]) <= set(files) | {"manifest.json"}
output = root / args.output
output.parent.mkdir(parents=True, exist_ok=True)
temporary = output.with_suffix(".zip.tmp")
try:
    with zipfile.ZipFile(temporary, "w", zipfile.ZIP_DEFLATED) as archive:
        for name in ["manifest.json", *files]:
            data = (json.dumps(manifest, indent=2) + "\n").encode() if name == "manifest.json" else files[name].read_bytes()
            info = zipfile.ZipInfo(f"{plugin_id}/{name}")
            info.create_system = 3
            info.compress_type = zipfile.ZIP_DEFLATED
            executable = name.endswith(".sh") or name.startswith("backend/")
            info.external_attr = (stat.S_IFREG | (0o755 if executable else 0o644)) << 16
            archive.writestr(info, data)
    temporary.replace(output)
finally:
    temporary.unlink(missing_ok=True)
print(output)
