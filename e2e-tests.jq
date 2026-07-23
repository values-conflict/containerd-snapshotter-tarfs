# input: none
# output: { image: { testName: { cmd: [ test, command ] } } }
def tests:
	reduce (
		[ "bash", "/usr/local/bin/bash" ],
		[ "golang", "/usr/local/go/bin/go" ],
		empty
	) as $img ({};
		.[$img[0]] = reduce (
			[ "echo", "hello" ],

			"find / -xdev -type f | wc -l",

			[ "wc", "-c", $img[1] ],

			empty
		) as $cmd ({};
			.[$cmd | if type == "array" then join(" ") else . end] = {
				cmd: ($cmd | if type == "array" then . else [ "sh", "-c", . ] end),
			}
		)
	)
;

# input: `tests` output
# output: shell code (string)
def shell:
	to_entries
	| [
		[
			"exec 3>&2 # copy stderr to fd 3 so we can default 'timed command' output to fd 3 but overwrite it for timings without losing fd 2 for normal error output",
			"_time() { local start end; start=$(date +%s%N); ( \"$@\" ) >&3; end=$(date +%s%N); echo \"$(( (end - start) / 1000000 ))\"; }",
			"export ms=0 # will now stay exported for future jq runs, even after modification",
			@sh "json=\(.[].value = { msPull: 0, msTest: {} } | from_entries + { du: { total: 0 } } | tojson)",
			empty
		]
		| join("\n")
	] + map(
		.key as $img
		| .value

		| [
			@sh "export img=\($img)",
			@sh "ms=$(_time docker pull \($img))",
			"json=$(jq <<<\"$json\" --compact-output '.[env.img].msPull = env.ms')",

			(
				to_entries[]
				| .key as $test
				| .value
				| ([ "docker", "run", "--rm", $img, .cmd[] | @sh ] | join(" ")) as $dockerRun
				| [
					@sh "export test=\($test)",

					"\($dockerRun) > /dev/null # throwaway run to warm caches",

					"total=0 count=10",
					"for (( i = 0; i < count; i++ )); do",
					"\tms=$(_time \($dockerRun) 3> /dev/null)",
					"\t(( total += ms )) || :",
					"done",
					"ms=$(( total / count ))",
					"json=$(jq <<<\"$json\" --compact-output '.[env.img].msTest[env.test] = env.ms')",

					empty
				]
				| join("\n")
			),

			empty
		]
		| join("\n\n")
	) + [
		"du=$(sudo du -bcsx /var/lib/containerd /var/lib/docker)",
		"export du",
		"jq <<<\"$json\" --tab '.du = (env.du | split(\"\\n\") | map(capture(\"^(?<value>.+)[[:space:]]+(?<key>.+)$\")) | from_entries)' | tee e2e-tests.json",

		empty
	]
	| join("\n\n")
;

# input: { "title": { e2e-tests.json }, "title": { e2e-tests.json }, ... ]
# output: markdown string
def summary:
	# input: [ [ ... ], [ ... ], ... ]
	# output: markdown table as a string
	def markdown_table:
		map(map(
			# https://stackoverflow.com/a/43070960/433558
			gsub("[|]"; "\\|")
		))
		| (
			transpose
			| map(
				map(length)
				| max
			)
		) as $columns
		| map(
			[ $columns, . ]
			| transpose
			| map(
				.[0] as $width
				| .[1] // ""
				| . + (" " * ($width - length))
			)
			| "| " + join(" | ") + " |"
		)
		| join("\n")
	;

	def unique_unsorted:
		# https://unix.stackexchange.com/a/738744/153467
		reduce .[] as $a ([]; if IN(.[]; $a) then . else . += [$a] end)
	;

	keys_unsorted as $keys

	| (
		[ .[] | keys_unsorted[] ]
		| unique_unsorted - [ "du" ]
	) as $images

	| [
		"## e2e: \($keys | join(" vs "))",

		(
			$images[] as $image

			| (
				[ .[][$image]?.msTest | keys_unsorted[] ]
				| unique_unsorted
			) as $tests

			| (
				"### \($image)",

				(
					[
						[ "", $keys[] ],
						[ "---", ($keys[] | "---:") ],
						[ "pull (ms)", .[$keys[]][$image].msPull // "unk" ],
						(
							$tests[] as $test
							| [ "avg `\($test)` ×10 (ms)", .[$keys[]][$image].msTest[$test] // "unk" ]
						),
						empty
					]
					| markdown_table
				),

				empty
			)
		),

		"### `du -bcsx`",
		(
			[
				[ "", $keys[] ],
				[ "---", ($keys[] | "---:") ],
				(
					([ .[].du | keys_unsorted[] ] | unique_unsorted[]) as $path
					| [ { total: "total" }[$path] // "`\($path)`", .[$keys[]].du[$path] // "unk" ]
				),
				empty
			]
			| markdown_table
		),

		empty
	] | join("\n\n")
;
