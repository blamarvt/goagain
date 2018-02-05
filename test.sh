#!/bin/bash

set -e -x

cd "$(dirname "$0")"

findproc() {
    set +x
    find "/proc" -mindepth 2 -maxdepth 2 -name "exe" -lname "$PWD/$1" 2>"/dev/null" |
    cut -d"/" -f"3"
    set -x
}

pushd "example/single"
go build
./single &

sleep 2

pgrep -f "./single"

for _ in _ _
do
    sleep 1
    pkill -HUP -f "./single"
    sleep 2
    pgrep -f "./single"
done

[ "$(nc "127.0.0.1" "48879")" = "Hello, world!" ]

pkill -TERM -f "./single"

sleep 2

pgrep -f "./single"


popd
