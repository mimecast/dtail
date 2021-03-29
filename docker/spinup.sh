#!/bin/bash

declare -i NUM_INSTANCES=$1
declare -i BASE_PORT=2222

echo > serverlist.txt

for (( i=0; i < $NUM_INSTANCES; i++ )); do
    port=$[ BASE_PORT + i + 1 ]
	docker run -d --name dserver-serv$i --hostname serv$i -p $port:2222 dserver:develop
	echo localhost:$port >> serverlist.txt
done
