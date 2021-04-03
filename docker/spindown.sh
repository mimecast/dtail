#!/bin/bash

declare -i NUM_INSTANCES=$1
declare -i BASE_PORT=2222

rm serverlist.txt &>/dev/null

for (( i=0; i < $NUM_INSTANCES; i++ )); do
    port=$[ BASE_PORT + i + 1 ]
	docker stop dserver-serv$i 
	docker rm dserver-serv$i 
done
