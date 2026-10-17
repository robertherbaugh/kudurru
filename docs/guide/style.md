# Writing rules for this guide

This guide uses Simplified Technical English (ASD-STE100). This page tells
authors how to keep the guide in that style. Readers do not need it.

NOTE: The specification and the approved dictionary are the property of
ASD. Get them from [asd-ste100.org](https://www.asd-ste100.org/). This page
does not replace them. It lists the rules that this project applies.

## Check your text

Run the checker before you commit a change to `docs/guide/`:

```sh
python3 hack/guide-lint.py
```

The checker finds the mistakes that a program can find. It does not check
words against the dictionary. A person must read the text too.

| Check | Limit |
|---|---|
| Sentence in a procedure step | 20 words at most |
| Sentence in other text | 25 words at most |
| Paragraph | 6 sentences at most |
| Contractions (for example `don't`) | not allowed |
| Words that are not approved (for example `should`, `via`, `etc.`) | not allowed |
| Future tense (`will`) | not allowed |
| Passive voice and `-ing` forms | listed when you use `--review` |

## Rules

1. Write short sentences. Put one idea in each sentence.
2. Use the active voice. Write "The server sends the answer", not "The
   answer is sent".
3. Use the present tense. Do not use the future tense.
4. Use articles (`a`, `an`, `the`). Do not omit them.
5. Write one instruction in each step. Start the step with a verb.
6. Put a warning before the step that it applies to.
7. Use one word for one meaning. Use the same word every time. Use the
   [terms](#terms) below.
8. Use a simple word. Write "use", not "utilize". Write "start", not
   "initiate".
9. Do not use idioms, slang or jokes. Do not use `e.g.`, `i.e.` or `etc.`
   Write "for example" and "that is".
10. Do not use a contraction. Write "do not", not "don't".
11. Write numbers as digits. Put a space between the number and the unit
    (`30 seconds`).
12. Use a noun cluster of three words at most. Write "the list of block
    lists", not "the block list list view".

## Warnings and notes

Use these labels. Put them on their own line.

| Label | Use it when |
|---|---|
| `CAUTION:` | A mistake can cause a loss of data or a loss of service. |
| `NOTE:` | The text gives extra information. |

This guide does not use `WARNING:`. In STE this label is only for a risk of
injury to people.

## Procedures

- Number the steps. Use one action in each step.
- Start each step with a verb in the imperative.
- Tell the reader what the result is after the last step, if the reader
  can see it.
- Start each chapter with **Before you start**. List the role and the
  items that the reader needs.

### Verbs for the dashboard

| Verb | Use it for |
|---|---|
| Open | A page, a dialog or a file. |
| Click | A button, a link, a tab or a menu item. |
| Select | An option in a list, a radio button or a check box. |
| Enter | Text that the reader types in a field. |
| Turn on, turn off | A switch. |

## Show text from the screen

- Write the text of a button, field or page in **bold**: **New list**.
- Write a file name, a command, a key or a value that the reader types
  in `code`.
- Write a menu path with an arrow: **Filtering** > **Lists**.
- Use the exact words that the dashboard shows. If the dashboard shows
  `Allowlist`, write `Allowlist`.

## Screenshots

- Every screenshot has a figure caption and an alternative text.
- The captions start with `Figure N.` and say what the reader sees.
- The dashboard screenshots show demo data from the dashboard mock
  (`dashboard/src/mocks`). The names and numbers on your system are different.
- Do not edit a screenshot by hand. Take it again with
  `dashboard/scripts/guide-screenshots.mjs` (see [Take the screenshots
  again](#take-the-screenshots-again)).

### Take the screenshots again

1. Start the dashboard with the demo data:

   ```sh
   cd dashboard && bun run dev:mock -- --port 5199 --strictPort
   ```

2. Start a node for the block page screenshots. Use the helper in
   `dashboard/e2e/support/kudurru.ts`. Set `KUDURRU_GUIDE_BLOCKPAGE` to the block
   page address of the node.
3. Run the script from the `dashboard` directory:

   ```sh
   node scripts/guide-screenshots.mjs
   ```

The header of the script lists the environment variables. The script
writes the PNG files to `docs/guide/images/`.

## Terms

Use these terms. Do not use the synonyms in the right column.

| Term | Meaning | Do not use |
|---|---|---|
| dashboard | The web interface of kudurru. | UI, console, GUI |
| node | One running kudurru server. | instance, host (for the server) |
| client | A device that sends DNS queries. | machine, endpoint |
| client group | A set of clients. | group of devices |
| query | A DNS question from a client. | request (for a DNS question) |
| request | A user asks an administrator to allow a name. | ticket |
| block list | A list of names that kudurru blocks. | blacklist, blocklist |
| allow list | A list of names that kudurru never blocks. | whitelist |
| policy | The rules that apply to a client group. | profile, rule set |
| zone | A part of the DNS name space that kudurru answers for. | domain (for a zone) |
| record | One DNS entry in a zone. | entry (for a DNS entry) |
| upstream | A DNS server that kudurru asks for an answer. | forwarder |
| administrator | A person who runs kudurru. | admin (except for the role and the account) |
| user | A person who uses the network. | end user |
| role | A set of permissions. | privilege level |

The names of dashboard pages, objects and settings are product names.
Keep them as they are: **Allowlist**, **Denylist**, `blocking.defaultResponse`.

## Task pages

- Give each page one task and a concrete result.
- Aim for 50 to 120 lines. Split unrelated procedures into separate pages.
- Start with **Before you start** and end with **Check the result**.
- Put a screenshot beside the step that it explains.
- Show the saved state and the running state separately when a restart is required.
- Keep deployment constraints and troubleshooting details in linked reference pages.
- Keep existing URLs and heading anchors when you move published material.

The walkthrough order is in `docs-site/hugo.yaml`. The site adds step
navigation to the First steps and DHCP sections.
To take only the DHCP screenshots again, use:

```sh
cd dashboard
KUDURRU_GUIDE_GROUPS=dhcp,dhcp-running node scripts/guide-screenshots.mjs
```
