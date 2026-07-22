// A published activity's entry file. It compiles in the client against the
// activity SDK surface, exactly like a node compiles against bombers/ui.
//
// Assets published alongside this file are downloaded at install and read from
// disk by path — see ctx.asset() in the client's activity context.
import { defineActivity, ui } from 'bombers';

export default defineActivity({
  id: 'sample-game',
  name: 'Sample Game',
  players: { min: 1, max: 4 },
  modes: ['solo', 'online'],
  render: (ctx) => (
    <ui.Stack>
      <ui.Text>Hello from a published activity.</ui.Text>
      <ui.Text>You are {ctx.isHost ? 'the host' : 'a guest'}.</ui.Text>
    </ui.Stack>
  ),
});
