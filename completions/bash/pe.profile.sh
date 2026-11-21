# Load Bash completions for pe and pe-index on minimal systems without bash-completion.
if [ -n "${BASH_VERSION:-}" ]; then
	case "$-" in
		*i*)
			if ! complete -p pe >/dev/null 2>&1 && [ -r /usr/share/bash-completion/completions/pe ]; then
				. /usr/share/bash-completion/completions/pe
			fi
			;;
	esac
fi
