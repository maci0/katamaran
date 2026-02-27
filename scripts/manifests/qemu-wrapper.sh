#!/bin/bash
new_args=()
replace_next_uuid=0
for arg in "$@"; do
    if [ "$replace_next_uuid" == "1" ]; then
        arg="${SRC_UUID}"
        replace_next_uuid=0
    elif [ "$arg" == "-uuid" ]; then
        replace_next_uuid=1
    fi
    arg=$(echo "$arg" | sed -E "s/id=vsock-[0-9]+/id=${SRC_VSOCK}/g")
    arg=$(echo "$arg" | sed -E "s/guest-cid=[0-9]+/guest-cid=${SRC_CID}/g")
    arg=$(echo "$arg" | sed -E "s/id=char-[a-f0-9]+/id=${SRC_CHAR}/g")
    arg=$(echo "$arg" | sed -E "s/chardev=char-[a-f0-9]+/chardev=${SRC_CHAR}/g")
    new_args+=("$arg")
done
exec /opt/kata/bin/qemu-system-x86_64.orig "${new_args[@]}" -incoming tcp:${NODE2_IP}:4444
