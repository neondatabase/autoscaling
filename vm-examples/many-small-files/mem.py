import re
import subprocess
import time
from datetime import datetime
import argparse

# Function to print the timestamp
def print_timestamp():
    timestamp = datetime.now().strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]  # Add milliseconds
    timestamp_line = f"#               Timestamp: {timestamp}               #"
    border = "#" * len(timestamp_line)
    print(f"\n{border}")
    print(timestamp_line)
    print(border + "\n")

# Function to parse /proc/meminfo
def parse_meminfo():
    meminfo_path = "/proc/meminfo"
    fields = {
        "MemTotal": "Total Memory",
        "MemFree": "Free Memory",
        "MemAvailable": "Available Memory",
        "Buffers": "Buffers",
        "Cached": "Cached Memory",
        "SwapCached": "Swap Cached",
        "Active": "Active Memory",
        "Inactive": "Inactive Memory",
        "SwapTotal": "Total Swap",
        "SwapFree": "Free Swap",
        "Slab": "Slab Memory",
        "SReclaimable": "Reclaimable Slab Memory",
        "SUnreclaim": "Unreclaimable Slab Memory",
    }

    meminfo_data = {}
    with open(meminfo_path, "r") as file:
        for line in file:
            key, value = line.split(":", 1)
            key = key.strip()
            if key in fields:
                meminfo_data[fields[key]] = value.strip()

    return meminfo_data

# Function to parse slabtop summary and first 3 entries
def parse_slabtop():
    try:
        slabtop_output = subprocess.check_output(["slabtop", "-o"], text=True)
        lines = slabtop_output.splitlines()

        summary_lines = []
        for line in lines:
            if line.startswith(" Active / Total"):
                summary_lines.append(line.strip())

        header_line = ""
        slab_entries = []
        capture = False
        for line in lines:
            if "OBJS ACTIVE  USE OBJ SIZE" in line:
                header_line = line.strip()
                capture = True
                continue
            if capture and len(line.strip()) > 0:
                slab_entries.append(line.strip())
                if len(slab_entries) == 3:
                    break

        return summary_lines, header_line, slab_entries

    except Exception as e:
        return [f"Error parsing slabtop: {e}"], "", []

# Function to parse /proc/zoneinfo
def parse_zoneinfo():
    zoneinfo_path = "/proc/zoneinfo"
    output = []
    current_node_zone = ""
    zone_data = {
        "Free Pages": 0,
        "Thresholds": {"min": 0, "low": 0, "high": 0},
        "Managed Pages": 0,
        "Anonymous Pages": {"active": 0, "inactive": 0},
        "File Pages": {"active": 0, "inactive": 0},
        "Unevictable Pages": 0,
    }

    def flush_zone_data():
        if current_node_zone:
            output.append(f"{current_node_zone}: "
                          f"Free={zone_data['Free Pages']} | "
                          f"Managed={zone_data['Managed Pages']} | "
                          f"Thresholds(min/low/high)={zone_data['Thresholds']['min']}/{zone_data['Thresholds']['low']}/{zone_data['Thresholds']['high']} | "
                          f"Anon(active/inactive)={zone_data['Anonymous Pages']['active']}/{zone_data['Anonymous Pages']['inactive']} | "
                          f"File(active/inactive)={zone_data['File Pages']['active']}/{zone_data['File Pages']['inactive']} | "
                          f"Unevictable={zone_data['Unevictable Pages']}")

    with open(zoneinfo_path, "r") as file:
        for line in file:
            node_match = re.match(r"Node (\d+), zone\s+(\w+)", line)
            if node_match:
                flush_zone_data()
                node, zone = node_match.groups()
                current_node_zone = f"Node {node}, Zone: {zone}"
                zone_data = {
                    "Free Pages": 0,
                    "Thresholds": {"min": 0, "low": 0, "high": 0},
                    "Managed Pages": 0,
                    "Anonymous Pages": {"active": 0, "inactive": 0},
                    "File Pages": {"active": 0, "inactive": 0},
                    "Unevictable Pages": 0,
                }
            else:
                if "pages free" in line:
                    zone_data["Free Pages"] = int(re.search(r"\d+", line).group())
                elif "managed" in line:
                    zone_data["Managed Pages"] = int(re.search(r"\d+", line).group())
                elif "min" in line:
                    zone_data["Thresholds"]["min"] = int(re.search(r"\d+", line).group())
                elif "low" in line:
                    zone_data["Thresholds"]["low"] = int(re.search(r"\d+", line).group())
                elif "high" in line:
                    zone_data["Thresholds"]["high"] = int(re.search(r"\d+", line).group())
                elif "nr_active_anon" in line:
                    zone_data["Anonymous Pages"]["active"] = int(re.search(r"\d+", line).group())
                elif "nr_inactive_anon" in line:
                    zone_data["Anonymous Pages"]["inactive"] = int(re.search(r"\d+", line).group())
                elif "nr_active_file" in line:
                    zone_data["File Pages"]["active"] = int(re.search(r"\d+", line).group())
                elif "nr_inactive_file" in line:
                    zone_data["File Pages"]["inactive"] = int(re.search(r"\d+", line).group())
                elif "nr_unevictable" in line:
                    zone_data["Unevictable Pages"] = int(re.search(r"\d+", line).group())

    flush_zone_data()
    return output

# Function to parse top memory-consuming processes
def parse_ps_aux():
    try:
        ps_output = subprocess.check_output(
            "ps aux --sort=-%mem | head -n 6", shell=True, text=True
        )
        lines = ps_output.splitlines()[1:]  # Skip header
        processes = []
        for line in lines:
            parts = line.split(None, 10)  # Split into columns
            if len(parts) >= 11:
                vsz = parts[4]
                rss = parts[5]
                stat = parts[7]
                time = parts[9]
                command = parts[10][:20]  # First 20 characters of the command
                processes.append(f"VSZ={vsz} | RSS={rss} | STAT={stat} | TIME={time} | CMD={command}")
        return processes
    except Exception as e:
        return [f"Error parsing ps aux: {e}"]

# Function to print system information
def print_system_info():
    print_timestamp()
    zoneinfo_output = parse_zoneinfo()
    slabtop_summary, slabtop_header, slabtop_top_entries = parse_slabtop()
    meminfo_output = parse_meminfo()
    ps_aux_output = parse_ps_aux()

    print("General Memory Info:")
    for key, value in meminfo_output.items():
        print(f"{key}: {value}")

    print("\nZoneinfo:")
    print("\n".join(zoneinfo_output))

    print("\nSlabtop Summary:")
    print("\n".join(slabtop_summary))

    print("\nSlabtop Top 3 Entries:")
    print(slabtop_header)
    print("\n".join(slabtop_top_entries))

    print("\nTop Memory-Consuming Processes:")
    print("\n".join(ps_aux_output))

# Main execution with command-line arguments
if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="System info script with optional delay.")
    parser.add_argument("-d", type=int, help="Delay in milliseconds for continuous printing.")
    args = parser.parse_args()

    if args.d:
        delay_seconds = args.d / 1000.0  # Convert milliseconds to seconds
        try:
            while True:
                print_system_info()
                # try:
                #     subprocess.run(["sh", "-c", "echo 1 > /proc/sys/vm/drop_caches"], check=True)
                # except subprocess.CalledProcessError as e:
                #     print(f"Failed to drop caches: {e}")
                time.sleep(delay_seconds)
        except KeyboardInterrupt:
            print("\nExiting...")
    else:
        print_system_info()
