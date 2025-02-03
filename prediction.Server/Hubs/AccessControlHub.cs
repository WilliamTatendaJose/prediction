using Microsoft.AspNetCore.SignalR;

namespace prediction.Server.Hubs
{
    public class AccessControlHub:Hub
    {
        public async Task SendMessageToAll(string message)
        {
            await Clients.All.SendAsync("NewMessage", message);
        }
    }
}
